package okdesk

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// newTestClient создаёт клиент, направленный на тестовый сервер.
func newTestClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	c, err := New(Config{
		BaseURL:        srv.URL,
		APIToken:       "secret-token",
		MaxAttempts:    2,
		InitialBackoff: time.Millisecond,
		MaxBackoff:     2 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// errorTransport — http.RoundTripper, всегда возвращающий заданную ошибку и
// считающий число попыток.
type errorTransport struct {
	err   error
	calls *int32
}

func (t errorTransport) RoundTrip(*http.Request) (*http.Response, error) {
	if t.calls != nil {
		atomic.AddInt32(t.calls, 1)
	}
	return nil, t.err
}

func TestRequestTransportErrorRedactsToken(t *testing.T) {
	const token = "super-secret-token"
	// net/http включает полный URL с api_token в текст транспортной ошибки.
	leaky := fmt.Errorf("Get %q: dial tcp: connection refused",
		"https://okdesk.example/api/v1/issues/list?api_token="+token)

	c, err := New(Config{
		BaseURL:        "https://okdesk.example",
		APIToken:       token,
		HTTPClient:     &http.Client{Transport: errorTransport{err: leaky}},
		MaxAttempts:    1,
		InitialBackoff: time.Millisecond,
		MaxBackoff:     time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = c.ListIssueParameters(context.Background())
	if err == nil {
		t.Fatal("ожидалась ошибка")
	}
	msg := err.Error()
	if strings.Contains(msg, token) {
		t.Fatalf("токен утёк в текст ошибки: %q", msg)
	}
	if !strings.Contains(msg, "***") {
		t.Fatalf("ожидалась маскировка токена: %q", msg)
	}
}

func TestRequestDoesNotRetryOnTLSError(t *testing.T) {
	var calls int32
	c, err := New(Config{
		BaseURL:        "https://okdesk.example",
		APIToken:       "secret-token",
		HTTPClient:     &http.Client{Transport: errorTransport{err: x509.UnknownAuthorityError{}, calls: &calls}},
		MaxAttempts:    4,
		InitialBackoff: time.Millisecond,
		MaxBackoff:     time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, err := c.ListIssueParameters(context.Background()); err == nil {
		t.Fatal("ожидалась ошибка")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("попыток = %d, ожидалась 1 (TLS-ошибка не повторяется)", got)
	}
}

func TestListIssueParameters(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/issues/parameters/list" {
			t.Errorf("путь = %q", r.URL.Path)
		}
		if got := r.URL.Query().Get("api_token"); got != "secret-token" {
			t.Errorf("api_token = %q", got)
		}
		w.Write([]byte(`{"parameters":[{"code":"phone","name":"Телефон","field_type":"ftstring","required":false}]}`))
	}))
	defer srv.Close()

	params, err := newTestClient(t, srv).ListIssueParameters(context.Background())
	if err != nil {
		t.Fatalf("ListIssueParameters: %v", err)
	}
	want := []IssueParameter{{Code: "phone", Name: "Телефон", FieldType: "ftstring", Required: false}}
	if !reflect.DeepEqual(params, want) {
		t.Fatalf("params = %+v, want %+v", params, want)
	}
}

func TestListOpenIssuesWithEmptyAttrQuery(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/issues/list" {
			t.Errorf("путь = %q", r.URL.Path)
		}
		q := r.URL.Query()
		if got := q["status_codes[]"]; !reflect.DeepEqual(got, []string{"opened", "inprogress"}) {
			t.Errorf("status_codes[] = %v", got)
		}
		if got := q.Get("custom_parameters[phone]"); got != "#null" {
			t.Errorf("custom_parameters[phone] = %q", got)
		}
		if got := q.Get("fields[issue]"); got != "id,title,status,contact,parameters" {
			t.Errorf("fields[issue] = %q", got)
		}
		if got := q.Get("page[number]"); got != "2" {
			t.Errorf("page[number] = %q", got)
		}
		if got := q.Get("page[size]"); got != "50" {
			t.Errorf("page[size] = %q", got)
		}
		// Значение атрибута другого типа (число) не должно ломать разбор.
		w.Write([]byte(`{"issues":[{"id":10,"title":"Заявка","contact":{"id":5,"name":"Иван"},"parameters":[{"code":"qty","value":1.5},{"code":"phone","value":""}]}]}`))
	}))
	defer srv.Close()

	issues, err := newTestClient(t, srv).ListOpenIssuesWithEmptyAttr(
		context.Background(), []string{"opened", "inprogress"}, "phone", 2, 50)
	if err != nil {
		t.Fatalf("ListOpenIssuesWithEmptyAttr: %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("len(issues) = %d, want 1", len(issues))
	}
	got := issues[0]
	if got.ID != 10 || got.Title != "Заявка" {
		t.Fatalf("issue = %+v", got)
	}
	if got.Contact == nil || got.Contact.ID != 5 || got.Contact.Name != "Иван" {
		t.Fatalf("contact = %+v", got.Contact)
	}
	if len(got.Parameters) != 2 || got.Parameters[0].Value != "1.5" {
		t.Fatalf("parameters = %+v", got.Parameters)
	}
}

func TestGetContact(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/contacts/" {
			t.Errorf("путь = %q", r.URL.Path)
		}
		if got := r.URL.Query().Get("id"); got != "5" {
			t.Errorf("id = %q", got)
		}
		w.Write([]byte(`{"contacts":[{"id":5,"name":"Иван","phone":"+70000000000","mobile_phone":"+71111111111"}]}`))
	}))
	defer srv.Close()

	contact, err := newTestClient(t, srv).GetContact(context.Background(), 5)
	if err != nil {
		t.Fatalf("GetContact: %v", err)
	}
	if contact.ID != 5 || contact.Phone != "+70000000000" || contact.MobilePhone != "+71111111111" {
		t.Fatalf("contact = %+v", contact)
	}
}

func TestGetContactRootObject(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/contacts/" {
			t.Errorf("путь = %q", r.URL.Path)
		}
		if got := r.URL.Query().Get("id"); got != "5" {
			t.Errorf("id = %q", got)
		}
		w.Write([]byte(`{"id":5,"name":"Иван","phone":"+70000000000","mobile_phone":"+71111111111"}`))
	}))
	defer srv.Close()

	contact, err := newTestClient(t, srv).GetContact(context.Background(), 5)
	if err != nil {
		t.Fatalf("GetContact: %v", err)
	}
	if contact.ID != 5 || contact.Phone != "+70000000000" || contact.MobilePhone != "+71111111111" {
		t.Fatalf("contact = %+v", contact)
	}
}

func TestGetContactNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"contacts":[]}`))
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv).GetContact(context.Background(), 5)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestSetIssueParameter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %q", r.Method)
		}
		if r.URL.Path != "/api/v1/issues/10/parameters" {
			t.Errorf("путь = %q", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		var payload struct {
			CustomParameters map[string]string `json:"custom_parameters"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatalf("тело запроса: %v", err)
		}
		if payload.CustomParameters["phone"] != "+70000000000" {
			t.Errorf("custom_parameters = %v", payload.CustomParameters)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := newTestClient(t, srv).SetIssueParameter(context.Background(), 10, "phone", "+70000000000"); err != nil {
		t.Fatalf("SetIssueParameter: %v", err)
	}
}

func TestGetIssueDescription(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/issues/10" {
			t.Errorf("путь = %q", r.URL.Path)
		}
		if got := r.URL.Query().Get("api_token"); got != "secret-token" {
			t.Errorf("api_token = %q", got)
		}
		w.Write([]byte(`{"id":10,"title":"Заявка","description":"Контактный номер: +79207939333"}`))
	}))
	defer srv.Close()

	got, err := newTestClient(t, srv).GetIssueDescription(context.Background(), 10)
	if err != nil {
		t.Fatalf("GetIssueDescription: %v", err)
	}
	if got != "Контактный номер: +79207939333" {
		t.Fatalf("description = %q", got)
	}
}

func TestRequestRetriesOn5xx(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		w.Write([]byte(`{"contacts":[{"id":5}]}`))
	}))
	defer srv.Close()

	client := newTestClient(t, srv)
	if _, err := client.GetContact(context.Background(), 5); err != nil {
		t.Fatalf("GetContact: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("calls = %d, want 2", got)
	}
}

func TestAPIErrorRedactsToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "token=secret-token invalid", http.StatusUnauthorized)
	}))
	defer srv.Close()

	client, err := New(Config{BaseURL: srv.URL, APIToken: "secret-token", MaxAttempts: 1})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = client.GetContact(context.Background(), 5)
	if err == nil {
		t.Fatal("ожидалась ошибка")
	}
	if strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("токен утёк в текст ошибки: %v", err)
	}
	if !strings.Contains(err.Error(), "***") {
		t.Fatalf("ожидалась маскировка токена: %v", err)
	}
}

func TestNewValidatesRequiredFields(t *testing.T) {
	if _, err := New(Config{APIToken: "t"}); err == nil {
		t.Fatal("ожидалась ошибка при пустом base_url")
	}
	if _, err := New(Config{BaseURL: "https://example.okdesk.ru"}); err == nil {
		t.Fatal("ожидалась ошибка при пустом api_token")
	}
}
