package sdwan

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/netascode/go-sdwan"
)

// fakeVManage imitates the parts of the vManage API the manager uses,
// including session handling: once a session ends, every request gets HTTP
// 200 with the HTML login page, as vManage does.
type fakeVManage struct {
	mu       sync.Mutex
	sessions map[string]bool
	next     int
	logins   int
	// running is the number of task polls that still report a running task.
	running int
	// noTaskID makes activate/deactivate answer without a task id.
	noTaskID bool
	// tasksBroken makes the task list return the login page even with a
	// valid session.
	tasksBroken bool
	// calls lists "METHOD path" of the API calls made with a valid session.
	calls []string
}

const loginPage = "<html><head><title>Cisco vManage</title></head><body>login</body></html>"

func newFakeVManage(t *testing.T) (*fakeVManage, *httptest.Server) {
	t.Helper()
	f := &fakeVManage{sessions: map[string]bool{}}
	ts := httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(ts.Close)
	return f, ts
}

// expire ends all sessions, like an idle timeout on vManage.
func (f *fakeVManage) expire() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessions = map[string]bool{}
}

func (f *fakeVManage) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, _ = io.Copy(io.Discard, r.Body)

	if r.Method == http.MethodPost && r.URL.Path == "/j_security_check" {
		f.next++
		f.logins++
		id := fmt.Sprintf("s%d", f.next)
		f.sessions[id] = true
		http.SetCookie(w, &http.Cookie{Name: "JSESSIONID", Value: id, Path: "/"})
		return // success: empty body
	}
	c, err := r.Cookie("JSESSIONID")
	if err != nil || !f.sessions[c.Value] {
		w.Header().Set("Content-Type", "text/html")
		_, _ = fmt.Fprint(w, loginPage)
		return
	}
	if r.URL.Path == "/dataservice/client/token" {
		_, _ = fmt.Fprint(w, "token-"+c.Value)
		return
	}
	if r.Header.Get("X-XSRF-TOKEN") != "token-"+c.Value {
		http.Error(w, "bad token", http.StatusForbidden)
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/dataservice")
	f.calls = append(f.calls, r.Method+" "+path)
	w.Header().Set("Content-Type", "application/json")
	switch {
	case path == "/client/about":
		_, _ = fmt.Fprint(w, `{"data":{"version":"20.15.6"}}`)
	case path == tasksEndpoint:
		switch {
		case f.tasksBroken:
			w.Header().Set("Content-Type", "text/html")
			_, _ = fmt.Fprint(w, loginPage)
		case f.running > 0:
			f.running--
			_, _ = fmt.Fprint(w, `{"runningTasks":[{"processId":"p1"}]}`)
		default:
			_, _ = fmt.Fprint(w, `{"runningTasks":[]}`)
		}
	case strings.HasPrefix(path, "/template/policy/vsmart/activate/"),
		strings.HasPrefix(path, "/template/policy/vsmart/deactivate/"):
		if f.noTaskID {
			_, _ = fmt.Fprint(w, `{}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"id":"vsmart_policy_config-1"}`)
	case r.Method == http.MethodPost && path == "/template/policy/list/dataprefix/":
		_, _ = fmt.Fprint(w, `{"listId":"dp-1"}`)
	case (r.Method == http.MethodPut || r.Method == http.MethodDelete) && strings.HasPrefix(path, "/template/policy/list/dataprefix/"):
		_, _ = fmt.Fprint(w, `{}`)
	case strings.HasPrefix(path, "/template/policy/definition/approute/"):
		if r.Method == http.MethodGet {
			_, _ = fmt.Fprint(w, `{"definitionId":"ar-1","name":"SDWANOperatorApproutePolicy","sequences":[]}`)
			return
		}
		_, _ = fmt.Fprint(w, `{}`)
	default:
		http.Error(w, "unexpected "+r.Method+" "+path, http.StatusNotFound)
	}
}

func (f *fakeVManage) state() (logins int, calls []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.logins, slices.Clone(f.calls)
}

func newTestManager(t *testing.T, url string) *sdwanManager {
	t.Helper()
	// No retries: the fake never fails transiently, and go-sdwan's backoff
	// would add seconds per failed request.
	c, err := sdwan.NewClient(url, "admin", "secret", true, sdwan.MaxRetries(0))
	if err != nil {
		t.Fatal(err)
	}
	return &sdwanManager{
		client: c,
		objectCache: map[string]map[string]string{
			"central":    {CentralizedPolicyName: "cp-1"},
			"dataprefix": {},
			"approute":   {ApproutePolicyName: "ar-1"},
		},
		log:              logr.Discard(),
		taskWaitTimeout:  time.Second,
		taskPollInterval: time.Millisecond,
	}
}

func upsert(m *sdwanManager) error {
	return m.HandleUpsertEvent("default", "llamacpp-backends", []string{"10.10.1.159", "10.10.0.22"}, 50052, "TCP", "biz-internet")
}

// An expired vManage session made the operator hang in waitTasksFinish
// (runningTasks read as "" instead of "[]") after a deactivate that silently
// did nothing. The manager must log in again and apply the change.
func TestUpsertAfterSessionExpiry(t *testing.T) {
	t.Parallel()
	f, ts := newFakeVManage(t)
	m := newTestManager(t, ts.URL)

	if err := upsert(m); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	f.expire()

	done := make(chan error, 1)
	go func() { done <- upsert(m) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("upsert after session expiry: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("upsert after session expiry hangs")
	}

	logins, calls := f.state()
	if logins != 2 {
		t.Fatalf("expected a second login after expiry, got %d logins", logins)
	}
	last := calls[len(calls)/2:]
	for _, want := range []string{
		"POST /template/policy/vsmart/deactivate/cp-1",
		"PUT /template/policy/list/dataprefix/dp-1",
		"PUT /template/policy/definition/approute/ar-1",
		"POST /template/policy/vsmart/activate/cp-1",
	} {
		if !slices.Contains(last, want) {
			t.Fatalf("%q not called after expiry; calls: %q", want, calls)
		}
	}
}

func TestDeleteAfterSessionExpiry(t *testing.T) {
	t.Parallel()
	f, ts := newFakeVManage(t)
	m := newTestManager(t, ts.URL)
	if err := upsert(m); err != nil {
		t.Fatal(err)
	}
	f.expire()

	if err := m.HandleDeleteEvent("default", "llamacpp-backends"); err != nil {
		t.Fatalf("delete after session expiry: %v", err)
	}
	if logins, calls := f.state(); logins != 2 || !slices.Contains(calls, "DELETE /template/policy/list/dataprefix/dp-1") {
		t.Fatalf("delete not applied after re-login: logins=%d calls=%q", logins, calls)
	}
}

// If logging in again does not help, nothing must be changed: in particular
// the central policy must not be deactivated.
func TestUpsertFailsBeforeChangesWhenSessionCannotBeRestored(t *testing.T) {
	t.Parallel()
	f, ts := newFakeVManage(t)
	f.tasksBroken = true
	m := newTestManager(t, ts.URL)

	err := upsert(m)
	if !errors.Is(err, errInvalidResponse) {
		t.Fatalf("expected errInvalidResponse, got %v", err)
	}
	if _, calls := f.state(); slices.ContainsFunc(calls, func(c string) bool { return strings.HasPrefix(c, "POST /template/") || strings.HasPrefix(c, "PUT ") }) {
		t.Fatalf("changes made despite an invalid session: %q", calls)
	}
}

func TestWaitTasksFinishWaitsForRunningTasks(t *testing.T) {
	t.Parallel()
	f, ts := newFakeVManage(t)
	f.running = 3
	m := newTestManager(t, ts.URL)

	if err := m.waitTasksFinish(); err != nil {
		t.Fatal(err)
	}
	_, calls := f.state()
	if n := countCalls(calls, "GET "+tasksEndpoint); n != 4 {
		t.Fatalf("expected 4 task polls (3 running, 1 done), got %d", n)
	}
}

func TestWaitTasksFinishTimesOut(t *testing.T) {
	t.Parallel()
	f, ts := newFakeVManage(t)
	f.running = 1 << 30
	m := newTestManager(t, ts.URL)
	m.taskWaitTimeout = 50 * time.Millisecond

	err := m.waitTasksFinish()
	if err == nil || !strings.Contains(err.Error(), "did not finish within 50ms") {
		t.Fatalf("expected a timeout error, got %v", err)
	}
}

func TestWaitTasksFinishLogsInAgain(t *testing.T) {
	t.Parallel()
	f, ts := newFakeVManage(t)
	m := newTestManager(t, ts.URL)
	if err := m.ensureSession(); err != nil {
		t.Fatal(err)
	}
	f.expire()

	if err := m.waitTasksFinish(); err != nil {
		t.Fatalf("waitTasksFinish after session expiry: %v", err)
	}
	if logins, _ := f.state(); logins != 2 {
		t.Fatalf("expected a second login, got %d", logins)
	}
}

func TestToggleRequiresTaskID(t *testing.T) {
	t.Parallel()
	f, ts := newFakeVManage(t)
	f.noTaskID = true
	m := newTestManager(t, ts.URL)

	if err := m.toggleCentralPolicy(false); !errors.Is(err, errInvalidResponse) {
		t.Fatalf("expected errInvalidResponse without a task id, got %v", err)
	}
}

func countCalls(calls []string, want string) int {
	n := 0
	for _, c := range calls {
		if c == want {
			n++
		}
	}
	return n
}
