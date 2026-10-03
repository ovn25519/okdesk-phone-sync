// Package okdesk реализует клиент REST API Okdesk, необходимый сервису
// okdesk-phone-sync: чтение схемы дополнительных атрибутов заявок, поиск
// открытых заявок с незаполненным атрибутом, получение контакта заявки и запись
// значения атрибута обратно в заявку.
//
// Клиент не логирует api_token: в журнал попадает только метод и путь запроса,
// а тело ответа при ошибке очищается от значения токена.
package okdesk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	defaultHTTPTimeout = 30 * time.Second
	defaultMaxAttempts = 3
	defaultBackoff     = 2 * time.Second
	defaultMaxBackoff  = 60 * time.Second
	maxResponseBytes   = 1 << 20 // 1 МиБ
	maxErrorBodyChars  = 300
)

// issueListFields — набор полей, которые запрашиваются у списка заявок:
// контакт и дополнительные атрибуты нужны, чтобы не делать лишний запрос к
// карточке заявки.
const issueListFields = "id,title,status,contact,parameters"

// ErrNotFound возвращается, когда запрошенный объект отсутствует в Okdesk.
var ErrNotFound = errors.New("okdesk: объект не найден")

// Config — параметры клиента Okdesk.
type Config struct {
	// BaseURL — базовый URL аккаунта Okdesk.
	BaseURL string
	// APIToken — токен API.
	APIToken string
	// HTTPClient — необязательный HTTP-клиент (по умолчанию с таймаутом).
	HTTPClient *http.Client
	// Logger — необязательный логгер (по умолчанию slog.Default).
	Logger *slog.Logger
	// MaxAttempts — общее число попыток на один логический запрос (включая
	// первую). Значение <= 0 заменяется значением по умолчанию.
	MaxAttempts int
	// InitialBackoff — задержка перед первой повторной попыткой.
	InitialBackoff time.Duration
	// MaxBackoff — верхняя граница экспоненциальной задержки повторов.
	MaxBackoff time.Duration
}

// Client — клиент REST API Okdesk.
type Client struct {
	baseURL        string
	apiToken       string
	http           *http.Client
	log            *slog.Logger
	maxAttempts    int
	initialBackoff time.Duration
	maxBackoff     time.Duration
}

// New создаёт клиент и проверяет обязательные параметры.
func New(cfg Config) (*Client, error) {
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		return nil, errors.New("okdesk: не задан base_url")
	}
	if strings.TrimSpace(cfg.APIToken) == "" {
		return nil, errors.New("okdesk: не задан api_token")
	}

	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: defaultHTTPTimeout}
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	maxAttempts := cfg.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = defaultMaxAttempts
	}
	initialBackoff := cfg.InitialBackoff
	if initialBackoff <= 0 {
		initialBackoff = defaultBackoff
	}
	maxBackoff := cfg.MaxBackoff
	if maxBackoff <= 0 {
		maxBackoff = defaultMaxBackoff
	}
	if maxBackoff < initialBackoff {
		maxBackoff = initialBackoff
	}

	return &Client{
		baseURL:        base,
		apiToken:       cfg.APIToken,
		http:           hc,
		log:            logger,
		maxAttempts:    maxAttempts,
		initialBackoff: initialBackoff,
		maxBackoff:     maxBackoff,
	}, nil
}

// IssueParameter — элемент схемы дополнительных атрибутов заявки.
type IssueParameter struct {
	Code      string
	Name      string
	FieldType string
	Required  bool
}

// ListIssueParameters возвращает схему дополнительных атрибутов заявок
// (GET /api/v1/issues/parameters/list).
func (c *Client) ListIssueParameters(ctx context.Context) ([]IssueParameter, error) {
	data, err := c.request(ctx, http.MethodGet, "/api/v1/issues/parameters/list", nil, nil)
	if err != nil {
		return nil, err
	}
	raws, err := extractList(data, "parameters", "issue_parameters", "issue_attributes")
	if err != nil {
		return nil, fmt.Errorf("okdesk: разбор схемы атрибутов заявок: %w", err)
	}

	params := make([]IssueParameter, 0, len(raws))
	for _, raw := range raws {
		var dto issueParameterDTO
		if json.Unmarshal(raw, &dto) != nil || dto.Code == "" {
			continue
		}
		params = append(params, IssueParameter{
			Code:      dto.Code,
			Name:      dto.Name,
			FieldType: dto.FieldType,
			Required:  dto.Required,
		})
	}
	return params, nil
}

// ContactRef — краткая ссылка на контакт в заявке.
type ContactRef struct {
	ID   int
	Name string
}

// IssueParameterValue — значение дополнительного атрибута конкретной заявки.
type IssueParameterValue struct {
	Code  string
	Value string
}

// Issue — открытая заявка в объёме, нужном для синхронизации.
type Issue struct {
	ID         int
	Title      string
	Contact    *ContactRef
	Parameters []IssueParameterValue
}

// ListOpenIssuesWithEmptyAttr возвращает страницу заявок в указанных статусах,
// у которых дополнительный атрибут attrCode пуст
// (GET /api/v1/issues/list с фильтром custom_parameters[attrCode]=#null).
//
// Нумерация страниц начинается с 1. Запрошенные поля включают контакт и
// дополнительные атрибуты, поэтому карточку заявки отдельно запрашивать не
// нужно.
func (c *Client) ListOpenIssuesWithEmptyAttr(
	ctx context.Context,
	statusCodes []string,
	attrCode string,
	page, pageSize int,
) ([]Issue, error) {
	q := url.Values{}
	for _, code := range statusCodes {
		q.Add("status_codes[]", code)
	}
	q.Set("custom_parameters["+attrCode+"]", "#null")
	q.Set("fields[issue]", issueListFields)
	q.Set("page[number]", strconv.Itoa(page))
	q.Set("page[size]", strconv.Itoa(pageSize))

	data, err := c.request(ctx, http.MethodGet, "/api/v1/issues/list", q, nil)
	if err != nil {
		return nil, err
	}
	raws, err := extractList(data, "issues")
	if err != nil {
		return nil, fmt.Errorf("okdesk: разбор списка заявок: %w", err)
	}

	issues := make([]Issue, 0, len(raws))
	for _, raw := range raws {
		var dto issueDTO
		if json.Unmarshal(raw, &dto) != nil || dto.ID <= 0 {
			continue
		}
		issue := Issue{ID: dto.ID, Title: dto.Title}
		if dto.Contact != nil && dto.Contact.ID > 0 {
			issue.Contact = &ContactRef{ID: dto.Contact.ID, Name: dto.Contact.Name}
		}
		for _, p := range dto.Parameters {
			issue.Parameters = append(issue.Parameters, IssueParameterValue{
				Code:  p.Code,
				Value: string(p.Value),
			})
		}
		issues = append(issues, issue)
	}
	return issues, nil
}

// Contact — контакт Okdesk в объёме, нужном для выбора телефона.
type Contact struct {
	ID          int
	Name        string
	Phone       string
	MobilePhone string
}

// GetContact возвращает контакт по идентификатору
// (GET /api/v1/contacts/?id=). Если контакт не найден, возвращается ErrNotFound.
//
// По документации метод возвращает один контакт корневым JSON-объектом. Для
// устойчивости поддерживается и ответ-обёртка {"contacts": [...]}.
func (c *Client) GetContact(ctx context.Context, id int) (*Contact, error) {
	q := url.Values{}
	q.Set("id", strconv.Itoa(id))

	data, err := c.request(ctx, http.MethodGet, "/api/v1/contacts/", q, nil)
	if err != nil {
		return nil, err
	}

	// Основной формат: корневой JSON-объект с контактом.
	var dto contactDTO
	if json.Unmarshal(data, &dto) == nil && dto.ID > 0 {
		if dto.ID != id {
			return nil, fmt.Errorf("%w: контакт id=%d", ErrNotFound, id)
		}
		return contactFromDTO(dto), nil
	}

	// Резервный формат: обёртка {"contacts": [...]}.
	raws, err := extractList(data, "contacts")
	if err != nil {
		return nil, fmt.Errorf("okdesk: разбор контакта: %w", err)
	}
	for _, raw := range raws {
		var item contactDTO
		if json.Unmarshal(raw, &item) != nil || item.ID <= 0 {
			continue
		}
		if item.ID == id {
			return contactFromDTO(item), nil
		}
	}
	return nil, fmt.Errorf("%w: контакт id=%d", ErrNotFound, id)
}

// contactFromDTO преобразует DTO контакта в доменную модель.
func contactFromDTO(dto contactDTO) *Contact {
	return &Contact{
		ID:          dto.ID,
		Name:        dto.Name,
		Phone:       dto.Phone,
		MobilePhone: dto.MobilePhone,
	}
}

// SetIssueParameter записывает значение дополнительного атрибута заявки
// (POST /api/v1/issues/{issue_id}/parameters). Операция идемпотентна: повторная
// запись того же значения безопасна.
func (c *Client) SetIssueParameter(ctx context.Context, issueID int, code, value string) error {
	path := "/api/v1/issues/" + strconv.Itoa(issueID) + "/parameters"
	body := setParametersRequest{CustomParameters: map[string]string{code: value}}
	_, err := c.request(ctx, http.MethodPost, path, nil, body)
	return err
}

// GetIssueDescription возвращает описание заявки
// (GET /api/v1/issues/{issue_id}).
//
// Описание не входит в список полей метода /issues/list, поэтому карточка
// запрашивается отдельно — только для заявок, у которых телефон не найден у
// контакта.
func (c *Client) GetIssueDescription(ctx context.Context, issueID int) (string, error) {
	path := "/api/v1/issues/" + strconv.Itoa(issueID)
	data, err := c.request(ctx, http.MethodGet, path, nil, nil)
	if err != nil {
		return "", err
	}
	var dto issueDetailsDTO
	if err := json.Unmarshal(data, &dto); err != nil {
		return "", fmt.Errorf("okdesk: разбор карточки заявки %d: %w", issueID, err)
	}
	return dto.Description, nil
}

// request выполняет логический запрос к Okdesk с повторными попытками на
// сетевых ошибках и ответах 5xx/429.
func (c *Client) request(ctx context.Context, method, path string, query url.Values, body any) ([]byte, error) {
	if query == nil {
		query = url.Values{}
	}
	query.Set("api_token", c.apiToken)

	var encoded []byte
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("okdesk: сериализация запроса %s: %w", path, err)
		}
		encoded = buf
	}

	var lastErr error
	for attempt := 1; attempt <= c.maxAttempts; attempt++ {
		if attempt > 1 {
			delay := c.backoff(attempt - 1)
			c.log.Warn("okdesk: повтор запроса",
				"method", method, "path", path, "attempt", attempt, "delay", delay, "error", lastErr)
			if err := sleepCtx(ctx, delay); err != nil {
				return nil, err
			}
		}

		data, retryable, err := c.attempt(ctx, method, path, query, encoded)
		if err == nil {
			return data, nil
		}
		lastErr = err
		if !retryable || ctx.Err() != nil {
			return data, err
		}
	}
	return nil, lastErr
}

// attempt выполняет одну HTTP-попытку и сообщает, имеет ли смысл повтор.
func (c *Client) attempt(ctx context.Context, method, path string, query url.Values, encoded []byte) ([]byte, bool, error) {
	target := c.baseURL + path + "?" + query.Encode()

	var reader io.Reader
	if encoded != nil {
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return nil, false, fmt.Errorf("okdesk: создание запроса %s: %w", path, err)
	}
	req.Header.Set("Accept", "application/json")
	if encoded != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	c.log.Debug("запрос к Okdesk", "method", method, "path", path)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, true, fmt.Errorf("okdesk: запрос %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, true, fmt.Errorf("okdesk: чтение ответа %s: %w", path, err)
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		apiErr := &APIError{
			Method:     method,
			Path:       path,
			StatusCode: resp.StatusCode,
			Body:       c.redact(string(data)),
		}
		retryable := resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests
		return data, retryable, apiErr
	}
	return data, false, nil
}

// backoff вычисляет задержку перед попыткой номер attempt (1 — первая повторная).
func (c *Client) backoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := c.initialBackoff
	for i := 1; i < attempt; i++ {
		d *= 2
		if d >= c.maxBackoff {
			return c.maxBackoff
		}
	}
	if d > c.maxBackoff {
		d = c.maxBackoff
	}
	return d
}

// redact удаляет значение api_token из строки (страховка на случай, если сервер
// вернёт его в теле ответа).
func (c *Client) redact(s string) string {
	if c.apiToken == "" {
		return s
	}
	return strings.ReplaceAll(s, c.apiToken, "***")
}

// APIError описывает неуспешный ответ Okdesk.
type APIError struct {
	Method     string
	Path       string
	StatusCode int
	Body       string
}

func (e *APIError) Error() string {
	body := strings.TrimSpace(e.Body)
	if len(body) > maxErrorBodyChars {
		body = body[:maxErrorBodyChars] + "…"
	}
	if body == "" {
		return fmt.Sprintf("okdesk: %s %s вернул %d", e.Method, e.Path, e.StatusCode)
	}
	return fmt.Sprintf("okdesk: %s %s вернул %d: %s", e.Method, e.Path, e.StatusCode, body)
}

// sleepCtx ждёт d или отмены контекста.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// --- DTO и вспомогательные функции -----------------------------------------

// flexibleString принимает значение атрибута Okdesk любого скалярного типа и
// приводит его к строке. Это защищает разбор заявки от атрибутов других типов
// (число, флаг), которые тоже присутствуют в массиве parameters.
type flexibleString string

func (f *flexibleString) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		*f = ""
		return nil
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return err
		}
		*f = flexibleString(s)
		return nil
	}
	*f = flexibleString(string(trimmed))
	return nil
}

type issueParameterDTO struct {
	Code      string `json:"code"`
	Name      string `json:"name"`
	FieldType string `json:"field_type"`
	Required  bool   `json:"required"`
}

type contactRefDTO struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

type parameterValueDTO struct {
	Code  string         `json:"code"`
	Value flexibleString `json:"value"`
}

type issueDTO struct {
	ID         int                 `json:"id"`
	Title      string              `json:"title"`
	Contact    *contactRefDTO      `json:"contact"`
	Parameters []parameterValueDTO `json:"parameters"`
}

type issueDetailsDTO struct {
	ID          int    `json:"id"`
	Description string `json:"description"`
}

type contactDTO struct {
	ID          int    `json:"id"`
	Name        string `json:"name"`
	Phone       string `json:"phone"`
	MobilePhone string `json:"mobile_phone"`
}

type setParametersRequest struct {
	CustomParameters map[string]string `json:"custom_parameters"`
}

// extractList извлекает массив объектов из ответа: как из обёртки вида
// {"contacts": [...]}, так и из массива в корне документа.
func extractList(data []byte, keys ...string) ([]json.RawMessage, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, nil
	}
	if trimmed[0] == '[' {
		var arr []json.RawMessage
		if err := json.Unmarshal(trimmed, &arr); err != nil {
			return nil, err
		}
		return arr, nil
	}

	var obj map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &obj); err != nil {
		return nil, err
	}
	for _, key := range keys {
		raw, ok := obj[key]
		if !ok {
			continue
		}
		var arr []json.RawMessage
		if err := json.Unmarshal(raw, &arr); err != nil {
			return nil, err
		}
		return arr, nil
	}
	return nil, nil
}
