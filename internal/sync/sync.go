// Package sync реализует демон синхронизации: периодически находит открытые
// заявки Okdesk с незаполненным дополнительным атрибутом, берёт телефон
// контакта заявки и записывает его в атрибут.
//
// Проход идемпотентен: заявка, у которой атрибут уже заполнен, больше не
// попадает в выборку. Ошибка на одной заявке не прерывает проход.
package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"okdesk-phone-sync/internal/okdesk"
	"okdesk-phone-sync/internal/phone"
)

// Значения по умолчанию.
const (
	defaultPollInterval = 5 * time.Minute
	defaultPageSize     = 50
)

// Client — подмножество клиента Okdesk, необходимое демону. Позволяет
// подменять клиент в тестах.
type Client interface {
	ListIssueParameters(ctx context.Context) ([]okdesk.IssueParameter, error)
	ListOpenIssuesWithEmptyAttr(ctx context.Context, statusCodes []string, attrCode string, page, pageSize int) ([]okdesk.Issue, error)
	GetContact(ctx context.Context, id int) (*okdesk.Contact, error)
	GetIssueDescription(ctx context.Context, issueID int) (string, error)
	SetIssueParameter(ctx context.Context, issueID int, code, value string) error
}

// Config — параметры демона.
type Config struct {
	// PhoneAttrCode — код дополнительного атрибута заявки, который заполняется.
	PhoneAttrCode string
	// StatusCodes — коды статусов, считающихся открытыми.
	StatusCodes []string
	// PhonePriority — порядок выбора телефона контакта (mobile_phone, phone).
	PhonePriority []string
	// PageSize — размер страницы при чтении заявок.
	PageSize int
	// PollInterval — период между проходами.
	PollInterval time.Duration
	// RequestDelay — пауза между последовательными запросами к Okdesk.
	RequestDelay time.Duration
	// DryRun — ничего не записывать в Okdesk, только сообщать в журнал.
	DryRun bool
	// VerifyAttr — проверять атрибут в схеме на старте.
	VerifyAttr bool
	// SearchInDescription — искать телефон в описании заявки, если у контакта
	// его нет.
	SearchInDescription bool
}

// Stats — итоги одного прохода.
type Stats struct {
	// Found — сколько заявок просмотрено.
	Found int
	// Updated — сколько атрибутов заполнено (номером из контакта или из описания).
	Updated int
	// UpdatedFromText — из них заполнено номером, найденным в описании заявки.
	UpdatedFromText int
	// WouldUpdate — сколько заявок было бы обновлено в режиме dry-run.
	WouldUpdate int
	// WouldUpdateFromText — из них по номеру, найденному в описании заявки.
	WouldUpdateFromText int
	// SkippedNoContact — заявки без контакта.
	SkippedNoContact int
	// SkippedNoPhone — заявки, у контакта которых нет телефона.
	SkippedNoPhone int
	// SkippedHasValue — заявки, у которых атрибут уже заполнен.
	SkippedHasValue int
	// Errors — количество ошибок обработки.
	Errors int
}

// Runner выполняет проходы синхронизации.
type Runner struct {
	client Client
	cfg    Config
	log    *slog.Logger

	// contacts — кэш телефонов контактов в рамках одного прохода. Пустое
	// значение означает «у контакта нет телефона» и тоже кэшируется.
	contacts map[int]string

	// sleep используется для пауз; вынесен для тестируемости.
	sleep func(context.Context, time.Duration) error
}

// New создаёт демон, подставляя значения по умолчанию.
func New(client Client, cfg Config, logger *slog.Logger) *Runner {
	if logger == nil {
		logger = slog.Default()
	}
	if cfg.PageSize <= 0 {
		cfg.PageSize = defaultPageSize
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = defaultPollInterval
	}
	return &Runner{
		client: client,
		cfg:    cfg,
		log:    logger,
		sleep:  sleepCtx,
	}
}

// Run проверяет атрибут и выполняет проходы до отмены контекста. Первый проход
// запускается сразу при старте.
func (r *Runner) Run(ctx context.Context) error {
	if err := r.verify(ctx); err != nil {
		return err
	}

	ticker := time.NewTicker(r.cfg.PollInterval)
	defer ticker.Stop()

	r.pass(ctx)

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			r.pass(ctx)
		}
	}
}

// RunOnce выполняет проверку атрибута и ровно один проход (режим -once).
func (r *Runner) RunOnce(ctx context.Context) (Stats, error) {
	if err := r.verify(ctx); err != nil {
		return Stats{}, err
	}
	return r.pass(ctx), nil
}

// verify проверяет, что phone_attr_code присутствует в схеме атрибутов заявок.
// Если VerifyAttr выключен, проверка пропускается.
func (r *Runner) verify(ctx context.Context) error {
	if !r.cfg.VerifyAttr {
		return nil
	}
	params, err := r.client.ListIssueParameters(ctx)
	if err != nil {
		return fmt.Errorf("проверка атрибута %q: %w", r.cfg.PhoneAttrCode, err)
	}
	for _, p := range params {
		if p.Code == r.cfg.PhoneAttrCode {
			r.log.Info("атрибут заявки найден",
				"code", p.Code, "name", p.Name, "field_type", p.FieldType)
			return nil
		}
	}
	return fmt.Errorf("атрибут заявки %q не найден в схеме; проверьте okdesk.phone_attr_code", r.cfg.PhoneAttrCode)
}

// pass выполняет один проход и возвращает статистику.
func (r *Runner) pass(ctx context.Context) Stats {
	start := time.Now()
	r.contacts = make(map[int]string)
	stats := Stats{}

pageLoop:
	for page := 1; ; page++ {
		if ctx.Err() != nil {
			break
		}
		issues, err := r.client.ListOpenIssuesWithEmptyAttr(
			ctx, r.cfg.StatusCodes, r.cfg.PhoneAttrCode, page, r.cfg.PageSize)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			stats.Errors++
			r.log.Error("не удалось получить список заявок", "page", page, "error", err)
			break
		}
		if len(issues) == 0 {
			break
		}

		for _, issue := range issues {
			if ctx.Err() != nil {
				break pageLoop
			}
			r.processIssue(ctx, issue, &stats)
		}

		if len(issues) < r.cfg.PageSize {
			break
		}
		if err := r.pause(ctx); err != nil {
			break
		}
	}

	r.log.Info("проход завершён",
		"dry_run", r.cfg.DryRun,
		"found", stats.Found,
		"updated", stats.Updated,
		"updated_from_text", stats.UpdatedFromText,
		"would_update", stats.WouldUpdate,
		"would_update_from_text", stats.WouldUpdateFromText,
		"skipped_no_contact", stats.SkippedNoContact,
		"skipped_no_phone", stats.SkippedNoPhone,
		"skipped_has_value", stats.SkippedHasValue,
		"errors", stats.Errors,
		"duration", time.Since(start).Round(time.Millisecond))
	return stats
}

// processIssue обрабатывает одну заявку.
func (r *Runner) processIssue(ctx context.Context, issue okdesk.Issue, stats *Stats) {
	stats.Found++

	// Локальная перепроверка: серверный фильтр #null может вернуть заявку,
	// у которой атрибут уже заполнен (например, при расхождении поведения).
	if hasParameterValue(issue.Parameters, r.cfg.PhoneAttrCode) {
		stats.SkippedHasValue++
		return
	}

	value, fromText, err := r.resolvePhone(ctx, issue, stats)
	if err != nil {
		stats.Errors++
		r.log.Error("не удалось определить телефон", "issue_id", issue.ID, "error", err)
		return
	}
	if value == "" {
		return // причина пропуска уже учтена в resolvePhone
	}

	source := "contact"
	if fromText {
		source = "description"
	}

	if r.cfg.DryRun {
		stats.WouldUpdate++
		if fromText {
			stats.WouldUpdateFromText++
		}
		r.log.Info("dry-run: атрибут был бы заполнен",
			"issue_id", issue.ID, "phone", value, "source", source)
		return
	}

	if err := r.client.SetIssueParameter(ctx, issue.ID, r.cfg.PhoneAttrCode, value); err != nil {
		stats.Errors++
		r.log.Error("не удалось заполнить атрибут",
			"issue_id", issue.ID, "code", r.cfg.PhoneAttrCode, "error", err)
		return
	}
	stats.Updated++
	if fromText {
		stats.UpdatedFromText++
	}
	r.log.Info("атрибут заполнен", "issue_id", issue.ID, "phone", value, "source", source)

	if err := r.pause(ctx); err != nil {
		return
	}
}

// resolvePhone определяет телефон для заявки: сначала у контакта, затем — если
// включён поиск в описании — в тексте описания. Второе значение равно true,
// если номер взят из описания.
//
// Если телефон не найден, функция увеличивает счётчики пропусков и возвращает
// пустую строку.
func (r *Runner) resolvePhone(ctx context.Context, issue okdesk.Issue, stats *Stats) (string, bool, error) {
	hasContact := issue.Contact != nil && issue.Contact.ID > 0

	if hasContact {
		value, err := r.contactPhone(ctx, issue.Contact.ID)
		if err != nil {
			return "", false, fmt.Errorf("контакт id=%d: %w", issue.Contact.ID, err)
		}
		if value != "" {
			return value, false, nil
		}
		r.log.Warn("у контакта нет телефона", "issue_id", issue.ID, "contact_id", issue.Contact.ID)
	} else {
		stats.SkippedNoContact++
		r.log.Warn("заявка без контакта", "issue_id", issue.ID, "title", issue.Title)
	}

	if !r.cfg.SearchInDescription {
		if hasContact {
			stats.SkippedNoPhone++
		}
		return "", false, nil
	}

	description, err := r.client.GetIssueDescription(ctx, issue.ID)
	if err != nil {
		return "", false, fmt.Errorf("описание заявки id=%d: %w", issue.ID, err)
	}
	if err := r.pause(ctx); err != nil {
		return "", false, err
	}
	if found, ok := phone.Extract(description); ok {
		r.log.Info("телефон найден в описании заявки", "issue_id", issue.ID, "phone", found)
		return found, true, nil
	}

	stats.SkippedNoPhone++
	r.log.Warn("телефон не найден ни у контакта, ни в описании", "issue_id", issue.ID)
	return "", false, nil
}

// contactPhone возвращает телефон контакта с учётом приоритета полей и кэша
// прохода.
func (r *Runner) contactPhone(ctx context.Context, contactID int) (string, error) {
	if phone, ok := r.contacts[contactID]; ok {
		return phone, nil
	}

	contact, err := r.client.GetContact(ctx, contactID)
	if err != nil {
		if errors.Is(err, okdesk.ErrNotFound) {
			r.log.Warn("контакт не найден", "contact_id", contactID)
			r.contacts[contactID] = ""
			return "", nil
		}
		return "", err
	}

	phone := pickPhone(contact, r.cfg.PhonePriority)
	r.contacts[contactID] = phone
	if err := r.pause(ctx); err != nil {
		return "", err
	}
	return phone, nil
}

// pause делает паузу между запросами к Okdesk.
func (r *Runner) pause(ctx context.Context) error {
	if r.cfg.RequestDelay <= 0 {
		return nil
	}
	return r.sleep(ctx, r.cfg.RequestDelay)
}

// pickPhone выбирает первое непустое поле телефона контакта согласно priority и
// приводит его к единому виду +7XXXXXXXXXX. Если номер не распознан, возвращает
// значение как есть, чтобы не потерять данные.
func pickPhone(contact *okdesk.Contact, priority []string) string {
	if contact == nil {
		return ""
	}
	for _, field := range priority {
		var raw string
		switch field {
		case "mobile_phone":
			raw = contact.MobilePhone
		case "phone":
			raw = contact.Phone
		default:
			continue
		}
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		if value, ok := phone.Normalize(raw); ok {
			return value
		}
		return raw
	}
	return ""
}

// hasParameterValue сообщает, заполнен ли атрибут code в списке атрибутов заявки.
func hasParameterValue(params []okdesk.IssueParameterValue, code string) bool {
	for _, p := range params {
		if p.Code == code && strings.TrimSpace(p.Value) != "" {
			return true
		}
	}
	return false
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
