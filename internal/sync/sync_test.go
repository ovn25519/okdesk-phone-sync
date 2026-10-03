package sync

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"okdesk-phone-sync/internal/okdesk"
)

type setCall struct {
	issueID int
	code    string
	value   string
}

type fakeClient struct {
	params     []okdesk.IssueParameter
	paramErr   error
	pages      map[int][]okdesk.Issue
	contacts   map[int]*okdesk.Contact
	contactErr error

	contactCalls int
	setCalls     []setCall
	setErr       error

	descriptions     map[int]string
	descriptionErr   error
	descriptionCalls int
}

func (f *fakeClient) ListIssueParameters(context.Context) ([]okdesk.IssueParameter, error) {
	return f.params, f.paramErr
}

func (f *fakeClient) ListOpenIssuesWithEmptyAttr(_ context.Context, _ []string, _ string, page, _ int) ([]okdesk.Issue, error) {
	return f.pages[page], nil
}

func (f *fakeClient) GetContact(_ context.Context, id int) (*okdesk.Contact, error) {
	f.contactCalls++
	if f.contactErr != nil {
		return nil, f.contactErr
	}
	contact, ok := f.contacts[id]
	if !ok {
		return nil, okdesk.ErrNotFound
	}
	return contact, nil
}

func (f *fakeClient) SetIssueParameter(_ context.Context, issueID int, code, value string) error {
	f.setCalls = append(f.setCalls, setCall{issueID: issueID, code: code, value: value})
	return f.setErr
}

func (f *fakeClient) GetIssueDescription(_ context.Context, issueID int) (string, error) {
	f.descriptionCalls++
	if f.descriptionErr != nil {
		return "", f.descriptionErr
	}
	return f.descriptions[issueID], nil
}

// newRunner собирает демон на фейковом клиенте с отключёнными паузами.
func newRunner(f *fakeClient, cfg Config) *Runner {
	if cfg.PhoneAttrCode == "" {
		cfg.PhoneAttrCode = "phone"
	}
	if cfg.PhonePriority == nil {
		cfg.PhonePriority = []string{"mobile_phone", "phone"}
	}
	if cfg.StatusCodes == nil {
		cfg.StatusCodes = []string{"opened"}
	}
	r := New(f, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	r.sleep = func(context.Context, time.Duration) error { return nil }
	return r
}

func TestRunOnceFillsMobilePhone(t *testing.T) {
	f := &fakeClient{
		pages: map[int][]okdesk.Issue{
			1: {{ID: 10, Contact: &okdesk.ContactRef{ID: 5, Name: "Иван"}}},
		},
		contacts: map[int]*okdesk.Contact{
			5: {ID: 5, MobilePhone: "+71111111111", Phone: "+70000000000"},
		},
	}

	stats, err := newRunner(f, Config{}).RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if stats.Updated != 1 {
		t.Fatalf("Updated = %d, want 1", stats.Updated)
	}
	want := []setCall{{issueID: 10, code: "phone", value: "+71111111111"}}
	if len(f.setCalls) != 1 || f.setCalls[0] != want[0] {
		t.Fatalf("setCalls = %+v, want %+v", f.setCalls, want)
	}
}

func TestRunOncePriorityFallsBackToPhone(t *testing.T) {
	f := &fakeClient{
		pages:    map[int][]okdesk.Issue{1: {{ID: 10, Contact: &okdesk.ContactRef{ID: 5}}}},
		contacts: map[int]*okdesk.Contact{5: {ID: 5, Phone: "+70000000000"}},
	}

	stats, err := newRunner(f, Config{}).RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if stats.Updated != 1 || f.setCalls[0].value != "+70000000000" {
		t.Fatalf("setCalls = %+v, stats = %+v", f.setCalls, stats)
	}
}

func TestRunOnceSkipsNoContact(t *testing.T) {
	f := &fakeClient{pages: map[int][]okdesk.Issue{1: {{ID: 10}}}}

	stats, err := newRunner(f, Config{}).RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if stats.SkippedNoContact != 1 || f.contactCalls != 0 || len(f.setCalls) != 0 {
		t.Fatalf("stats = %+v, contactCalls = %d, setCalls = %+v", stats, f.contactCalls, f.setCalls)
	}
}

func TestRunOnceSkipsNoPhone(t *testing.T) {
	f := &fakeClient{
		pages:    map[int][]okdesk.Issue{1: {{ID: 10, Contact: &okdesk.ContactRef{ID: 5}}}},
		contacts: map[int]*okdesk.Contact{5: {ID: 5}},
	}

	stats, err := newRunner(f, Config{}).RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if stats.SkippedNoPhone != 1 || len(f.setCalls) != 0 {
		t.Fatalf("stats = %+v, setCalls = %+v", stats, f.setCalls)
	}
}

func TestRunOnceSkipsAlreadyFilled(t *testing.T) {
	f := &fakeClient{
		pages: map[int][]okdesk.Issue{1: {{
			ID:         10,
			Contact:    &okdesk.ContactRef{ID: 5},
			Parameters: []okdesk.IssueParameterValue{{Code: "phone", Value: "+70000000000"}},
		}}},
	}

	stats, err := newRunner(f, Config{}).RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if stats.SkippedHasValue != 1 || f.contactCalls != 0 || len(f.setCalls) != 0 {
		t.Fatalf("stats = %+v, contactCalls = %d, setCalls = %+v", stats, f.contactCalls, f.setCalls)
	}
}

func TestRunOnceDryRunDoesNotWrite(t *testing.T) {
	f := &fakeClient{
		pages:    map[int][]okdesk.Issue{1: {{ID: 10, Contact: &okdesk.ContactRef{ID: 5}}}},
		contacts: map[int]*okdesk.Contact{5: {ID: 5, MobilePhone: "+71111111111"}},
	}

	stats, err := newRunner(f, Config{DryRun: true}).RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if stats.WouldUpdate != 1 || stats.Updated != 0 || len(f.setCalls) != 0 {
		t.Fatalf("stats = %+v, setCalls = %+v", stats, f.setCalls)
	}
}

func TestRunOnceCachesContacts(t *testing.T) {
	f := &fakeClient{
		pages: map[int][]okdesk.Issue{1: {
			{ID: 10, Contact: &okdesk.ContactRef{ID: 5}},
			{ID: 11, Contact: &okdesk.ContactRef{ID: 5}},
		}},
		contacts: map[int]*okdesk.Contact{5: {ID: 5, MobilePhone: "+71111111111"}},
	}

	stats, err := newRunner(f, Config{}).RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if stats.Updated != 2 || f.contactCalls != 1 {
		t.Fatalf("stats = %+v, contactCalls = %d, want Updated=2, contactCalls=1", stats, f.contactCalls)
	}
}

func TestRunOncePagination(t *testing.T) {
	f := &fakeClient{
		pages: map[int][]okdesk.Issue{
			1: {{ID: 10, Contact: &okdesk.ContactRef{ID: 5}}, {ID: 11, Contact: &okdesk.ContactRef{ID: 6}}},
			2: {{ID: 12, Contact: &okdesk.ContactRef{ID: 7}}},
		},
		contacts: map[int]*okdesk.Contact{
			5: {ID: 5, Phone: "+70000000005"},
			6: {ID: 6, Phone: "+70000000006"},
			7: {ID: 7, Phone: "+70000000007"},
		},
	}

	stats, err := newRunner(f, Config{PageSize: 2}).RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if stats.Found != 3 || stats.Updated != 3 {
		t.Fatalf("stats = %+v, want Found=3, Updated=3", stats)
	}
}

func TestRunOnceVerifyMissingAttr(t *testing.T) {
	f := &fakeClient{params: []okdesk.IssueParameter{{Code: "other", Name: "Другое"}}}

	_, err := newRunner(f, Config{VerifyAttr: true}).RunOnce(context.Background())
	if err == nil {
		t.Fatal("ожидалась ошибка проверки атрибута")
	}
}

func TestRunOnceContactErrorCountsAsError(t *testing.T) {
	f := &fakeClient{
		pages:      map[int][]okdesk.Issue{1: {{ID: 10, Contact: &okdesk.ContactRef{ID: 5}}}},
		contactErr: errors.New("boom"),
	}

	stats, err := newRunner(f, Config{}).RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if stats.Errors != 1 || len(f.setCalls) != 0 {
		t.Fatalf("stats = %+v, setCalls = %+v", stats, f.setCalls)
	}
}

func TestRunOnceFillsFromDescription(t *testing.T) {
	f := &fakeClient{
		pages:        map[int][]okdesk.Issue{1: {{ID: 10, Contact: &okdesk.ContactRef{ID: 5}}}},
		contacts:     map[int]*okdesk.Contact{5: {ID: 5}},
		descriptions: map[int]string{10: "Контактный номер: +79207939333"},
	}

	stats, err := newRunner(f, Config{SearchInDescription: true}).RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if stats.Updated != 1 || stats.UpdatedFromText != 1 {
		t.Fatalf("stats = %+v, want Updated=1, UpdatedFromText=1", stats)
	}
	if len(f.setCalls) != 1 || f.setCalls[0].value != "+79207939333" {
		t.Fatalf("setCalls = %+v", f.setCalls)
	}
	if f.descriptionCalls != 1 {
		t.Fatalf("descriptionCalls = %d, want 1", f.descriptionCalls)
	}
}

func TestRunOnceSearchInDescriptionDisabledByDefault(t *testing.T) {
	f := &fakeClient{
		pages:        map[int][]okdesk.Issue{1: {{ID: 10, Contact: &okdesk.ContactRef{ID: 5}}}},
		contacts:     map[int]*okdesk.Contact{5: {ID: 5}},
		descriptions: map[int]string{10: "Контактный номер: +79207939333"},
	}

	stats, err := newRunner(f, Config{}).RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if stats.SkippedNoPhone != 1 || stats.Updated != 0 || len(f.setCalls) != 0 {
		t.Fatalf("stats = %+v, setCalls = %+v", stats, f.setCalls)
	}
	if f.descriptionCalls != 0 {
		t.Fatalf("descriptionCalls = %d, want 0", f.descriptionCalls)
	}
}

func TestRunOnceNoContactFillsFromDescription(t *testing.T) {
	f := &fakeClient{
		pages:        map[int][]okdesk.Issue{1: {{ID: 10}}},
		descriptions: map[int]string{10: "Номер: 79139165288"},
	}

	stats, err := newRunner(f, Config{SearchInDescription: true}).RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if stats.Updated != 1 || stats.UpdatedFromText != 1 || stats.SkippedNoContact != 1 {
		t.Fatalf("stats = %+v", stats)
	}
	if len(f.setCalls) != 1 || f.setCalls[0].value != "+79139165288" {
		t.Fatalf("setCalls = %+v", f.setCalls)
	}
}

func TestRunOnceDescriptionWithoutPhone(t *testing.T) {
	f := &fakeClient{
		pages:        map[int][]okdesk.Issue{1: {{ID: 10, Contact: &okdesk.ContactRef{ID: 5}}}},
		contacts:     map[int]*okdesk.Contact{5: {ID: 5}},
		descriptions: map[int]string{10: "Адрес: д. 4Б/2 кв 350"},
	}

	stats, err := newRunner(f, Config{SearchInDescription: true}).RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if stats.SkippedNoPhone != 1 || stats.Updated != 0 || len(f.setCalls) != 0 {
		t.Fatalf("stats = %+v, setCalls = %+v", stats, f.setCalls)
	}
}

func TestRunOnceDryRunTextDoesNotWrite(t *testing.T) {
	f := &fakeClient{
		pages:        map[int][]okdesk.Issue{1: {{ID: 10, Contact: &okdesk.ContactRef{ID: 5}}}},
		contacts:     map[int]*okdesk.Contact{5: {ID: 5}},
		descriptions: map[int]string{10: "Номер: 79882883180"},
	}

	stats, err := newRunner(f, Config{SearchInDescription: true, DryRun: true}).RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if stats.WouldUpdate != 1 || stats.WouldUpdateFromText != 1 || stats.Updated != 0 || len(f.setCalls) != 0 {
		t.Fatalf("stats = %+v, setCalls = %+v", stats, f.setCalls)
	}
}

func TestRunOnceNormalizesContactPhone(t *testing.T) {
	f := &fakeClient{
		pages:    map[int][]okdesk.Issue{1: {{ID: 10, Contact: &okdesk.ContactRef{ID: 5}}}},
		contacts: map[int]*okdesk.Contact{5: {ID: 5, MobilePhone: "79207939333"}},
	}

	stats, err := newRunner(f, Config{}).RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if stats.Updated != 1 || len(f.setCalls) != 1 || f.setCalls[0].value != "+79207939333" {
		t.Fatalf("stats = %+v, setCalls = %+v", stats, f.setCalls)
	}
}

func TestRunOnceKeepsUnrecognizedContactPhone(t *testing.T) {
	f := &fakeClient{
		pages:    map[int][]okdesk.Issue{1: {{ID: 10, Contact: &okdesk.ContactRef{ID: 5}}}},
		contacts: map[int]*okdesk.Contact{5: {ID: 5, Phone: "доб. 123"}},
	}

	stats, err := newRunner(f, Config{}).RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if stats.Updated != 1 || len(f.setCalls) != 1 || f.setCalls[0].value != "доб. 123" {
		t.Fatalf("stats = %+v, setCalls = %+v", stats, f.setCalls)
	}
}
