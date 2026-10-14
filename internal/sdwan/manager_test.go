package sdwan

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/netascode/go-sdwan"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
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
	// fail makes the API calls starting with this "METHOD path" fail.
	fail string
	// loginPageFor makes the API calls starting with this "METHOD path"
	// return the login page, as after a session ends.
	loginPageFor string
	// listsBroken makes the object lists return the login page even with a
	// valid session.
	listsBroken bool
	// vsmartPolicies is the data array of the vSmart policy list.
	vsmartPolicies string
	// dataPrefixLists maps the dataprefix lists by name to their id. Names
	// are truncated to maxListNameLen, as vManage does.
	dataPrefixLists map[string]string
	nextList        int
	// approute is the approute definition, as last PUT.
	approute string
	// calls lists "METHOD path" of the API calls made with a valid session.
	calls []string
}

const loginPage = "<html><head><title>Cisco vManage</title></head><body>login</body></html>"

func newFakeVManage(t *testing.T) (*fakeVManage, *httptest.Server) {
	t.Helper()
	f := &fakeVManage{
		sessions:        map[string]bool{},
		vsmartPolicies:  `[{"policyName":"` + CentralizedPolicyName + `","policyId":"cp-1"}]`,
		dataPrefixLists: map[string]string{},
		approute:        `{"definitionId":"ar-1","name":"SDWANOperatorApproutePolicy","type":"appRoute","sequences":[],"defaultAction":{"type":"slaClass"}}`,
	}
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
	body, _ := io.ReadAll(r.Body)

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
	if f.fail != "" && strings.HasPrefix(r.Method+" "+path, f.fail) {
		http.Error(w, "failing "+f.fail, http.StatusInternalServerError)
		return
	}
	if f.loginPageFor != "" && strings.HasPrefix(r.Method+" "+path, f.loginPageFor) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = fmt.Fprint(w, loginPage)
		return
	}
	if (r.Method == http.MethodPost || r.Method == http.MethodPut) && len(body) > 0 && !json.Valid(body) {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	isList := r.Method == http.MethodGet && (path == "/template/policy/vsmart" ||
		path == "/template/policy/list/dataprefix" || path == "/template/policy/definition/approute")
	if f.listsBroken && isList {
		w.Header().Set("Content-Type", "text/html")
		_, _ = fmt.Fprint(w, loginPage)
		return
	}
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
	case r.Method == http.MethodGet && path == "/template/policy/vsmart":
		_, _ = fmt.Fprint(w, `{"data":`+f.vsmartPolicies+`}`)
	case r.Method == http.MethodGet && path == "/template/policy/list/dataprefix":
		lists := []map[string]string{}
		for name, id := range f.dataPrefixLists {
			lists = append(lists, map[string]string{"name": name, "listId": id})
		}
		data, _ := json.Marshal(lists)
		_, _ = fmt.Fprintf(w, `{"data":%s}`, data)
	case r.Method == http.MethodGet && path == "/template/policy/definition/approute":
		_, _ = fmt.Fprint(w, `{"data":[{"name":"SDWANOperatorApproutePolicy","definitionId":"ar-1"}]}`)
	case strings.HasPrefix(path, "/template/policy/vsmart/activate/"),
		strings.HasPrefix(path, "/template/policy/vsmart/deactivate/"):
		if f.noTaskID {
			_, _ = fmt.Fprint(w, `{}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"id":"vsmart_policy_config-1"}`)
	case r.Method == http.MethodPost && path == "/template/policy/list/dataprefix/":
		name := gjson.GetBytes(body, "name").String()
		if len(name) > maxListNameLen {
			name = name[:maxListNameLen]
		}
		if _, ok := f.dataPrefixLists[name]; ok {
			http.Error(w, "duplicate list name "+name, http.StatusBadRequest)
			return
		}
		f.nextList++
		id := fmt.Sprintf("dp-%d", f.nextList)
		f.dataPrefixLists[name] = id
		_, _ = fmt.Fprintf(w, `{"listId":%q}`, id)
	case (r.Method == http.MethodPut || r.Method == http.MethodDelete) && strings.HasPrefix(path, "/template/policy/list/dataprefix/"):
		id := strings.TrimPrefix(path, "/template/policy/list/dataprefix/")
		name, ok := f.dataPrefixListName(id)
		if !ok {
			http.Error(w, "no dataprefix list "+id, http.StatusNotFound)
			return
		}
		if r.Method == http.MethodDelete {
			if gjson.Get(f.approute, `sequences.#(match.entries.0.ref=="`+id+`")`).Exists() {
				http.Error(w, "dataprefix list "+id+" is in use", http.StatusBadRequest)
				return
			}
			delete(f.dataPrefixLists, name)
		}
		_, _ = fmt.Fprint(w, `{}`)
	case strings.HasPrefix(path, "/template/policy/definition/approute/"):
		if r.Method == http.MethodGet {
			_, _ = fmt.Fprint(w, f.approute)
			return
		}
		f.approute = string(body)
		_, _ = fmt.Fprint(w, `{}`)
	default:
		http.Error(w, "unexpected "+r.Method+" "+path, http.StatusNotFound)
	}
}

func (f *fakeVManage) dataPrefixListName(id string) (string, bool) {
	for name, listID := range f.dataPrefixLists {
		if listID == id {
			return name, true
		}
	}
	return "", false
}

// setDataPrefixList changes a dataprefix list behind the operator's back, like
// an edit in the vManage UI. An empty id deletes the list.
func (f *fakeVManage) setDataPrefixList(name, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if id == "" {
		delete(f.dataPrefixLists, name)
		return
	}
	f.dataPrefixLists[name] = id
}

func (f *fakeVManage) lists() map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return maps.Clone(f.dataPrefixLists)
}

// addLegacyList adds a dataprefix list from before names were hashed, with
// approute sequences that use it.
func (f *fakeVManage) addLegacyList(t *testing.T, name, id string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dataPrefixLists[name] = id
	for i, direction := range []string{"source", "destination"} {
		rule, err := generateSeqRule(100+i, direction, id, "80", "6", "mpls")
		if err != nil {
			t.Fatal(err)
		}
		if f.approute, err = sjson.SetRaw(f.approute, "sequences.-1", rule); err != nil {
			t.Fatal(err)
		}
	}
}

// usesList tells if an approute sequence matches on the dataprefix list id.
func (f *fakeVManage) usesList(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return gjson.Get(f.approute, `sequences.#(match.entries.0.ref=="`+id+`")`).Exists()
}

func (f *fakeVManage) approuteDefinition() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.approute
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
	c, err := sdwan.NewClient(url, true, sdwan.WithLogin("admin", "secret"), sdwan.MaxRetries(0))
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

// Reading the cache stopped at the first vSmart policy, so the operator's own
// policy was found only when vManage listed it first.
func TestRefreshCacheFindsCentralPolicy(t *testing.T) {
	t.Parallel()
	const other = `{"policyName":"OtherPolicy","policyId":"cp-other"}`
	const own = `{"policyName":"` + CentralizedPolicyName + `","policyId":"cp-1"}`
	for _, tc := range []struct {
		name, policies string
		found          bool
	}{
		{"first", "[" + own + "," + other + "]", true},
		{"second", "[" + other + "," + own + "]", true},
		{"last of three", "[" + other + "," + other + "," + own + "]", true},
		{"missing", "[" + other + "]", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, ts := newFakeVManage(t)
			f.vsmartPolicies = tc.policies
			m := newTestManager(t, ts.URL)
			m.objectCache = map[string]map[string]string{}

			if err := m.refreshCache(); err != nil {
				t.Fatal(err)
			}
			id, ok := m.objectCache["central"][CentralizedPolicyName]
			if ok != tc.found || (tc.found && id != "cp-1") {
				t.Fatalf("central policy id = %q (found %v), want cp-1 (found %v)", id, ok, tc.found)
			}
		})
	}
}

// The object cache was read only at startup, so a dataprefix list deleted or
// recreated in vManage left a stale id behind: the next update went to a list
// that no longer existed.
func TestUpsertSeesListsChangedInVManage(t *testing.T) {
	t.Parallel()
	name := convertK8sNameToSdWan("default", "llamacpp-backends")
	for _, tc := range []struct {
		name, newID, want string
	}{
		{"deleted", "", "POST /template/policy/list/dataprefix/"},
		{"recreated", "dp-ui", "PUT /template/policy/list/dataprefix/dp-ui"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, ts := newFakeVManage(t)
			m := newTestManager(t, ts.URL)
			if err := upsert(m); err != nil {
				t.Fatalf("first upsert: %v", err)
			}
			_, before := f.state()
			f.setDataPrefixList(name, tc.newID)

			if err := upsert(m); err != nil {
				t.Fatalf("upsert after the list changed in vManage: %v", err)
			}
			_, calls := f.state()
			calls = calls[len(before):]
			if !slices.Contains(calls, tc.want) {
				t.Fatalf("%q not called; calls: %q", tc.want, calls)
			}
			if slices.Contains(calls, "PUT /template/policy/list/dataprefix/dp-1") {
				t.Fatalf("update sent to the stale list id; calls: %q", calls)
			}
		})
	}
}

// A cache refresh that reads the login page instead of a list must not empty
// the cache, which would make the operator create duplicate lists.
func TestUpsertFailsBeforeChangesWhenListsCannotBeRead(t *testing.T) {
	t.Parallel()
	f, ts := newFakeVManage(t)
	m := newTestManager(t, ts.URL)
	if err := upsert(m); err != nil {
		t.Fatal(err)
	}
	_, before := f.state()
	cache := m.objectCache
	f.mu.Lock()
	f.listsBroken = true
	f.mu.Unlock()

	if err := upsert(m); !errors.Is(err, errInvalidResponse) {
		t.Fatalf("expected errInvalidResponse, got %v", err)
	}
	_, calls := f.state()
	if calls = calls[len(before):]; slices.ContainsFunc(calls, func(c string) bool { return strings.HasPrefix(c, "POST /template/") || strings.HasPrefix(c, "PUT ") }) {
		t.Fatalf("changes made despite unreadable lists: %q", calls)
	}
	if !reflect.DeepEqual(m.objectCache, cache) {
		t.Fatalf("cache changed by a failed refresh: %#v, was %#v", m.objectCache, cache)
	}
}

// Lists were looked up by the first 20 characters of their name, so
// business-app-1 and business-app-10 shared a list: an update or delete of one
// hit the other.
func TestListsOfSimilarNamesStayApart(t *testing.T) {
	t.Parallel()
	f, ts := newFakeVManage(t)
	m := newTestManager(t, ts.URL)
	upsertApp := func(name, address string) {
		t.Helper()
		if err := m.HandleUpsertEvent("default", name, []string{address}, 8080, "TCP", "biz-internet"); err != nil {
			t.Fatalf("upsert %s: %v", name, err)
		}
	}

	upsertApp("business-app-1", "10.0.0.1")
	upsertApp("business-app-10", "10.0.0.10")
	lists := f.lists()
	if len(lists) != 2 {
		t.Fatalf("expected a list per service, got %v", lists)
	}
	id1 := lists[convertK8sNameToSdWan("default", "business-app-1")]
	id10 := lists[convertK8sNameToSdWan("default", "business-app-10")]
	if id1 == "" || id10 == "" || id1 == id10 {
		t.Fatalf("services do not have their own lists: %v", lists)
	}

	_, before := f.state()
	if err := m.HandleDeleteEvent("default", "business-app-10"); err != nil {
		t.Fatalf("delete business-app-10: %v", err)
	}
	_, calls := f.state()
	if calls = calls[len(before):]; !slices.Contains(calls, "DELETE /template/policy/list/dataprefix/"+id10) ||
		slices.Contains(calls, "DELETE /template/policy/list/dataprefix/"+id1) {
		t.Fatalf("delete of business-app-10 hit the wrong list; calls: %q", calls)
	}
	if lists := f.lists(); len(lists) != 1 || lists[convertK8sNameToSdWan("default", "business-app-1")] != id1 {
		t.Fatalf("business-app-1 lost its list: %v", lists)
	}
}

// A change that failed after the central policy was deactivated returned
// without activating it again, leaving the network-wide policy off.
func TestFailedChangeActivatesCentralPolicyAgain(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, fail string
		delete     bool
	}{
		{"upsert, dataprefix list", "POST /template/policy/list/dataprefix/", false},
		{"upsert, approute", "PUT /template/policy/definition/approute/", false},
		{"delete, approute", "PUT /template/policy/definition/approute/", true},
		{"delete, dataprefix list", "DELETE /template/policy/list/dataprefix/", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, ts := newFakeVManage(t)
			m := newTestManager(t, ts.URL)
			if tc.delete {
				if err := upsert(m); err != nil {
					t.Fatal(err)
				}
			}
			f.mu.Lock()
			f.fail = tc.fail
			f.mu.Unlock()
			_, before := f.state()

			var err error
			if tc.delete {
				err = m.HandleDeleteEvent("default", "llamacpp-backends")
			} else {
				err = upsert(m)
			}
			if err == nil || !strings.Contains(err.Error(), "StatusCode 500") {
				t.Fatalf("expected the failed call's error, got %v", err)
			}
			_, calls := f.state()
			calls = calls[len(before):]
			failed := slices.IndexFunc(calls, func(c string) bool { return strings.HasPrefix(c, tc.fail) })
			if failed < 0 || !slices.Contains(calls[failed:], "POST /template/policy/vsmart/activate/cp-1") {
				t.Fatalf("central policy not activated after the failed change; calls: %q", calls)
			}
		})
	}
}

// A delete of a service without a list has nothing to do: it must succeed,
// so that it is not retried, and not touch the central policy.
func TestDeleteOfUnknownListLeavesCentralPolicyAlone(t *testing.T) {
	t.Parallel()
	f, ts := newFakeVManage(t)
	m := newTestManager(t, ts.URL)

	if err := m.HandleDeleteEvent("default", "unknown"); err != nil {
		t.Fatalf("delete of a service without a list: %v", err)
	}
	if _, calls := f.state(); slices.ContainsFunc(calls, func(c string) bool { return strings.HasPrefix(c, "POST /template/policy/vsmart/") }) {
		t.Fatalf("central policy toggled for an unknown list; calls: %q", calls)
	}
}

// A failed deactivate was logged at info level, so it did not show up among
// the errors.
func TestFailedDeactivateIsLoggedAsError(t *testing.T) {
	t.Parallel()
	f, ts := newFakeVManage(t)
	f.fail = "POST /template/policy/vsmart/deactivate/"
	m := newTestManager(t, ts.URL)
	var mu sync.Mutex
	var errorLogs []string
	m.log = funcr.New(func(_, args string) {
		if strings.Contains(args, `"error"=`) {
			mu.Lock()
			defer mu.Unlock()
			errorLogs = append(errorLogs, args)
		}
	}, funcr.Options{})

	if err := upsert(m); err != nil {
		t.Fatalf("upsert must go ahead after a failed deactivate: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.ContainsFunc(errorLogs, func(l string) bool { return strings.Contains(l, "cannot deactivate central policy") }) {
		t.Fatalf("failed deactivate not logged as an error; error logs: %q", errorLogs)
	}
}

// The approute sequences were built by string concatenation: they ended in a
// stray "]}", and a tunnel annotation with a quote could inject fields.
func TestUpsertSendsValidApproute(t *testing.T) {
	t.Parallel()
	f, ts := newFakeVManage(t)
	m := newTestManager(t, ts.URL)

	if err := m.HandleUpsertEvent("default", "app", []string{"10.0.0.1"}, 8080, "TCP", "biz-internet mpls"); err != nil {
		t.Fatal(err)
	}
	def := f.approuteDefinition()
	if !gjson.Valid(def) {
		t.Fatalf("approute definition is not valid JSON: %s", def)
	}
	if got := gjson.Get(def, "defaultAction.type").String(); got != "slaClass" {
		t.Fatalf("approute definition lost its defaultAction: %s", def)
	}
	seqs := gjson.Get(def, "sequences").Array()
	if len(seqs) != 2 {
		t.Fatalf("expected 2 sequences, got %s", def)
	}
	for i, direction := range []string{"source", "destination"} {
		seq := seqs[i]
		if got := seq.Get("actions.0.parameter").String(); got != "biz-internet mpls" {
			t.Fatalf("sequence %d tunnel = %q", i, got)
		}
		if got := seq.Get("match.entries.0.field").String(); got != direction+"DataPrefixList" {
			t.Fatalf("sequence %d matches %q", i, got)
		}
		if got := seq.Get(`match.entries.#(field=="protocol").value`).String(); got != "6" {
			t.Fatalf("sequence %d protocol = %q", i, got)
		}
	}
}

func TestUpsertRejectsInvalidSpec(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, protocol, tunnel, address string
	}{
		{"tunnel with a quote", "TCP", `biz-internet"}],"x":"y`, "10.0.0.1"},
		{"empty tunnel", "TCP", "", "10.0.0.1"},
		{"address with a quote", "TCP", "biz-internet", `10.0.0.1"`},
		// An unknown protocol was sent to vManage as protocol 0.
		{"unknown protocol", "ICMP", "biz-internet", "10.0.0.1"},
		{"empty protocol", "", "biz-internet", "10.0.0.1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, ts := newFakeVManage(t)
			m := newTestManager(t, ts.URL)

			err := m.HandleUpsertEvent("default", "app", []string{tc.address}, 8080, tc.protocol, tc.tunnel)
			if !errors.Is(err, ErrInvalidSpec) {
				t.Fatalf("expected ErrInvalidSpec, got %v", err)
			}
			if _, calls := f.state(); len(calls) != 0 {
				t.Fatalf("vManage called for an invalid spec: %q", calls)
			}
		})
	}
}

// Before names were hashed, vManage kept long list names truncated to 20
// characters. Those lists and their approute sequences must be removed once
// the service has a list under its new name, or when the service is deleted.
func TestLegacyListsAreRemoved(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, service, legacy string
		delete                bool
	}{
		{"upsert, long name", "llamacpp-backends", "default___llamacpp-b", false},
		{"upsert, name of exactly 20 characters", "business-a", "default___business-a", false},
		{"delete, long name", "llamacpp-backends", "default___llamacpp-b", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, ts := newFakeVManage(t)
			f.addLegacyList(t, tc.legacy, "dp-legacy")
			m := newTestManager(t, ts.URL)

			var err error
			if tc.delete {
				err = m.HandleDeleteEvent("default", tc.service)
			} else {
				err = m.HandleUpsertEvent("default", tc.service, []string{"10.0.0.1"}, 8080, "TCP", "biz-internet")
			}
			if err != nil {
				t.Fatal(err)
			}
			lists := f.lists()
			if _, ok := lists[tc.legacy]; ok || f.usesList("dp-legacy") {
				t.Fatalf("legacy list not removed: lists %v, approute %s", lists, f.approuteDefinition())
			}
			newID, ok := lists[convertK8sNameToSdWan("default", tc.service)]
			if tc.delete == ok || (!tc.delete && !f.usesList(newID)) {
				t.Fatalf("service list wrong after the change: lists %v, approute %s", lists, f.approuteDefinition())
			}
		})
	}
}

// A service with a short name never had a legacy list: another service's
// lists must be left alone.
func TestShortNamesHaveNoLegacyList(t *testing.T) {
	t.Parallel()
	f, ts := newFakeVManage(t)
	f.addLegacyList(t, "default___app-backend", "dp-other")
	m := newTestManager(t, ts.URL)

	if err := m.HandleUpsertEvent("default", "app", []string{"10.0.0.1"}, 8080, "TCP", "biz-internet"); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.lists()["default___app-backend"]; !ok || !f.usesList("dp-other") {
		t.Fatalf("another service's list was touched: %v", f.lists())
	}
}

// An empty endpoint list made generateDataPrefixList panic. A service without
// endpoints must lose its list and sequences instead.
func TestUpsertWithoutEndpointsRemovesList(t *testing.T) {
	t.Parallel()
	f, ts := newFakeVManage(t)
	m := newTestManager(t, ts.URL)
	if err := upsert(m); err != nil {
		t.Fatal(err)
	}
	id := f.lists()[convertK8sNameToSdWan("default", "llamacpp-backends")]

	for i := range 2 { // the second time there is nothing left to remove
		if err := m.HandleUpsertEvent("default", "llamacpp-backends", []string{}, 50052, "TCP", "biz-internet"); err != nil {
			t.Fatalf("upsert %d without endpoints: %v", i+1, err)
		}
		if lists := f.lists(); len(lists) != 0 || f.usesList(id) {
			t.Fatalf("list not removed: lists %v, approute %s", lists, f.approuteDefinition())
		}
	}
}

func TestUpsertProtocolNumbers(t *testing.T) {
	t.Parallel()
	for protocol, want := range map[string]string{"TCP": "6", "UDP": "17", "SCTP": "132"} {
		t.Run(protocol, func(t *testing.T) {
			t.Parallel()
			f, ts := newFakeVManage(t)
			m := newTestManager(t, ts.URL)

			if err := m.HandleUpsertEvent("default", "app", []string{"10.0.0.1"}, 8080, protocol, "biz-internet"); err != nil {
				t.Fatal(err)
			}
			got := gjson.Get(f.approuteDefinition(), `sequences.#.match.entries.#(field=="protocol").value`).Array()
			if len(got) != 2 || got[0].String() != want || got[1].String() != want {
				t.Fatalf("protocol %s sent as %v, want %s", protocol, got, want)
			}
		})
	}
}

// After a session ends, go-sdwan reports the login page as a success with an
// empty body. A new list then got the id "", and an approute definition read
// as empty was written back as only the new sequences.
func TestUpsertChecksResponses(t *testing.T) {
	t.Parallel()
	for _, call := range []string{
		"POST /template/policy/list/dataprefix/",
		"GET /template/policy/definition/approute/",
	} {
		t.Run(call, func(t *testing.T) {
			t.Parallel()
			f, ts := newFakeVManage(t)
			f.loginPageFor = call
			m := newTestManager(t, ts.URL)
			def := f.approuteDefinition()

			if err := upsert(m); !errors.Is(err, errInvalidResponse) {
				t.Fatalf("expected errInvalidResponse, got %v", err)
			}
			if got := f.approuteDefinition(); got != def {
				t.Fatalf("approute definition changed: %s", got)
			}
		})
	}
}

// A single IPv6 endpoint of a dual-stack service failed the whole upsert, so
// the service lost its SD-WAN policy.
func TestUpsertSkipsIPv6Endpoints(t *testing.T) {
	t.Parallel()
	f, ts := newFakeVManage(t)
	m := newTestManager(t, ts.URL)

	if err := m.HandleUpsertEvent("default", "app", []string{"fd00::1", "10.0.0.1"}, 8080, "TCP", "biz-internet"); err != nil {
		t.Fatal(err)
	}
	id := f.lists()["default___app"]
	if id == "" || !f.usesList(id) {
		t.Fatalf("no list for the IPv4 endpoint: %v", f.lists())
	}

	// with only IPv6 endpoints left there is nothing to route
	if err := m.HandleUpsertEvent("default", "app", []string{"fd00::1"}, 8080, "TCP", "biz-internet"); err != nil {
		t.Fatal(err)
	}
	if lists := f.lists(); len(lists) != 0 || f.usesList(id) {
		t.Fatalf("list not removed without IPv4 endpoints: %v", lists)
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
