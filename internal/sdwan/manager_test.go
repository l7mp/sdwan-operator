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
	"strconv"
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
	// fail makes the API calls starting with this "METHOD path" fail with
	// failStatus, 500 if unset.
	fail       string
	failStatus int
	// rejectList makes POSTs and PUTs of the dataprefix list of this name
	// fail with 400, as vManage rejects a list it finds invalid.
	rejectList string
	// loginPageFor makes the API calls starting with this "METHOD path"
	// return the login page, as after a session ends.
	loginPageFor string
	// noApproute leaves the operator's approute definition out of the list.
	noApproute bool
	// listsBroken makes the object lists return the login page even with a
	// valid session.
	listsBroken bool
	// vsmartPolicies is the data array of the vSmart policy list.
	vsmartPolicies string
	// dataPrefixLists maps the dataprefix lists by name to their id. Names
	// are truncated to maxListNameLen, as vManage does.
	dataPrefixLists map[string]string
	nextList        int
	// listEntries holds the entries of the dataprefix lists by id, which
	// the list of lists includes unless noEntries is set.
	listEntries map[string]string
	noEntries   bool
	// approute is the approute definition, as last PUT.
	approute string
	// calls lists "METHOD path" of the API calls made with a valid session.
	calls []string
	// policyCalls lists "path?query body" of the activate and deactivate
	// calls.
	policyCalls []string
	// inactive tells if the central policy is deactivated. As the testbed's
	// vManage 20.15 did: a PUT while it is active opens an edit session
	// (lock) that affects the vSmart template tmpl-1; after a reactivate
	// (activate with isEdited) PUTs fail with POLICY0013; DELETE
	// /template/lock/<lock> discards the session and rolls the approute
	// definition back to its state when the session opened; a deactivate ends
	// the session. Assumed (python-viptela's flow, not observed): PUTs before
	// the push join the session, and pushing tmpl-1 (attachfeature) commits
	// it, unless sticky is set. noTemplates makes PUTs affect no template.
	inactive    bool
	lock        string
	pushed      bool
	nextLock    int
	sticky      bool
	noTemplates bool
	// featureTemplate makes tmpl-1 a feature template; by default it is a
	// CLI template (configType "file"), as the testbed's vSmart template is,
	// which the vSmart refuses to get through attachfeature.
	featureTemplate bool
	snapshot        string
	// attachFails makes the template push fail on the vSmart; attaches
	// lists the bodies of the attachfeature calls.
	attachFails  bool
	failedAttach bool
	attaches     []string
	// editResponse is the answer to the PUT being served, if it opened an
	// edit session.
	editResponse string
}

const loginPage = "<html><head><title>Cisco vManage</title></head><body>login</body></html>"

func newFakeVManage(t *testing.T) (*fakeVManage, *httptest.Server) {
	t.Helper()
	f := &fakeVManage{
		sessions:        map[string]bool{},
		vsmartPolicies:  `[{"policyName":"` + CentralizedPolicyName + `","policyId":"cp-1"}]`,
		dataPrefixLists: map[string]string{},
		listEntries:     map[string]string{},
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
		status := f.failStatus
		if status == 0 {
			status = http.StatusInternalServerError
		}
		http.Error(w, "failing "+f.fail, status)
		return
	}
	if f.rejectList != "" && (r.Method == http.MethodPost || r.Method == http.MethodPut) &&
		strings.HasPrefix(path, "/template/policy/list/dataprefix/") && gjson.GetBytes(body, "name").String() == f.rejectList {
		http.Error(w, `{"error":{"message":"invalid list"}}`, http.StatusBadRequest)
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
	if r.Method == http.MethodPut && strings.HasPrefix(path, "/template/policy/") {
		if f.lock != "" && f.pushed {
			http.Error(w, `{"error":{"message":"Failed to acquire lock, template or policy locked in edit mode.","code":"POLICY0013","details":"Failed to acquire lock for template type policy_definition with policy idcp-1","type":"error"}}`, http.StatusBadRequest)
			return
		}
		if !f.inactive {
			if f.lock == "" {
				f.nextLock++
				f.lock, f.pushed, f.snapshot = fmt.Sprintf("push_feature_template_configuration-%d", f.nextLock), false, f.approute
			}
			templates := `["tmpl-1"]`
			if f.noTemplates {
				templates = `[]`
			}
			f.editResponse = fmt.Sprintf(`{"processId":%q,"masterTemplatesAffected":%s}`, f.lock, templates)
		}
	}
	if r.Method == http.MethodDelete && strings.HasPrefix(path, "/template/lock/") {
		if id := strings.TrimPrefix(path, "/template/lock/"); id == f.lock {
			f.lock, f.approute = "", f.snapshot
		}
		_, _ = fmt.Fprint(w, `{}`)
		return
	}
	switch {
	case r.Method == http.MethodGet && path == "/template/device" && r.URL.Query().Get("feature") == "all":
		configType := "file"
		if f.featureTemplate {
			configType = "template"
		}
		_, _ = fmt.Fprintf(w, `{"data":[{"templateId":"tmpl-1","templateName":"vSmartManaged","configType":%q,"deviceType":"vsmart"}]}`, configType)
		return
	case r.Method == http.MethodGet && path == "/template/device/config/attached/tmpl-1":
		_, _ = fmt.Fprint(w, `{"data":[{"uuid":"vsmart-1","deviceIP":"1.1.1.3"}]}`)
		return
	case r.Method == http.MethodPost && path == "/template/device/config/input/":
		if gjson.GetBytes(body, "templateId").String() != "tmpl-1" || !gjson.GetBytes(body, "isEdited").Bool() {
			http.Error(w, "bad input request "+string(body), http.StatusBadRequest)
			return
		}
		_, _ = fmt.Fprint(w, `{"header":{},"data":[{"csv-status":"complete","csv-deviceId":"vsmart-1","csv-deviceIP":"1.1.1.3"}]}`)
		return
	case r.Method == http.MethodPost && (path == "/template/device/config/attachfeature" || path == "/template/device/config/attachcli"):
		f.attaches = append(f.attaches, path+" "+string(body))
		// the vSmart refuses a push of its CLI template as a feature template
		// (access-denied), and vManage then rolls the edit back
		if wrong := (path == "/template/device/config/attachcli") == f.featureTemplate; wrong {
			f.failedAttach = true
			if f.lock != "" {
				f.lock, f.approute = "", f.snapshot
			}
		} else if gjson.GetBytes(body, "deviceTemplateList.0.templateId").String() == "tmpl-1" && !f.sticky {
			f.lock = ""
		}
		_, _ = fmt.Fprintf(w, `{"id":"push_feature_template_configuration-attach-%d"}`, len(f.attaches))
		return
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/device/action/status/") && path != tasksEndpoint:
		if f.attachFails || f.failedAttach {
			f.failedAttach = false
			_, _ = fmt.Fprint(w, `{"summary":{"status":"done","count":{"Failure":1}},"data":[{"host-name":"vsmart1","status":"Failure","currentActivity":"Device failed to process request.  Error type : application Error tag : access-denied"}]}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"summary":{"status":"done","count":{"Success":1}},"data":[{"host-name":"vsmart1","status":"Success"}]}`)
		return
	}
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
		lists := []map[string]any{}
		for name, id := range f.dataPrefixLists {
			l := map[string]any{"name": name, "listId": id}
			if entries, ok := f.listEntries[id]; ok && !f.noEntries {
				l["entries"] = json.RawMessage(entries)
			}
			lists = append(lists, l)
		}
		data, _ := json.Marshal(lists)
		_, _ = fmt.Fprintf(w, `{"data":%s}`, data)
	case r.Method == http.MethodGet && path == "/template/policy/definition/approute":
		if f.noApproute {
			_, _ = fmt.Fprint(w, `{"data":[]}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"data":[{"name":"SDWANOperatorApproutePolicy","definitionId":"ar-1"}]}`)
	case strings.HasPrefix(path, "/template/policy/vsmart/activate/"),
		strings.HasPrefix(path, "/template/policy/vsmart/deactivate/"):
		f.policyCalls = append(f.policyCalls, path+"?"+r.URL.RawQuery+" "+string(body))
		f.inactive = strings.HasPrefix(path, "/template/policy/vsmart/deactivate/")
		if f.inactive {
			f.lock = ""
		} else if f.lock != "" {
			f.pushed = true
		}
		if f.noTaskID {
			_, _ = fmt.Fprint(w, `{}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"id":"vsmart_policy_config-1"}`)
	case r.Method == http.MethodPost && path == "/template/policy/list/dataprefix/":
		name := vManageStoredName(gjson.GetBytes(body, "name").String())
		if _, ok := f.dataPrefixLists[name]; ok {
			http.Error(w, "duplicate list name "+name, http.StatusBadRequest)
			return
		}
		f.nextList++
		id := fmt.Sprintf("dp-%d", f.nextList)
		f.dataPrefixLists[name] = id
		f.listEntries[id] = gjson.GetBytes(body, "entries").Raw
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
			delete(f.listEntries, id)
		} else {
			f.listEntries[id] = gjson.GetBytes(body, "entries").Raw
		}
		f.answerEdit(w)
	case strings.HasPrefix(path, "/template/policy/definition/approute/"):
		if r.Method == http.MethodGet {
			_, _ = fmt.Fprint(w, f.approute)
			return
		}
		f.approute = string(body)
		f.answerEdit(w)
	default:
		http.Error(w, "unexpected "+r.Method+" "+path, http.StatusNotFound)
	}
}

// answerEdit answers a PUT: with the edit session it opened, if any.
func (f *fakeVManage) answerEdit(w http.ResponseWriter) {
	if f.editResponse != "" {
		_, _ = fmt.Fprint(w, f.editResponse)
		f.editResponse = ""
		return
	}
	_, _ = fmt.Fprint(w, `{}`)
}

// vManageStoredName returns the name vManage 20.15 stores for a list: names
// longer than vManageMaxNameLen become a prefix, "-" and the Java hashCode.
func vManageStoredName(name string) string {
	if len(name) <= vManageMaxNameLen {
		return name
	}
	h := int64(javaStringHash(name))
	if h < 0 {
		h = -h
	}
	suffix := "-" + strconv.FormatInt(h, 10)
	return name[:vManageMaxNameLen-len(suffix)] + suffix
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

// upsertOne applies an upsert on its own.
func upsertOne(m Manager, namespace, name string, endpoints []string, targetPort int64, protocol, tunnel string) error {
	return m.Apply([]Change{{Namespace: namespace, Name: name, Endpoints: endpoints, TargetPort: targetPort, Protocol: protocol, Tunnel: tunnel}})[0]
}

// deleteOne applies a delete on its own.
func deleteOne(m Manager, namespace, name string) error {
	return m.Apply([]Change{{Namespace: namespace, Name: name, Delete: true}})[0]
}

func upsert(m *sdwanManager) error {
	return upsertOne(m, "default", "llamacpp-backends", []string{"10.10.1.159", "10.10.0.22"}, 50052, "TCP", "biz-internet")
}

// upsertChanged changes the endpoints and the port of the service of upsert,
// so that both its list and its sequences change.
func upsertChanged(m *sdwanManager) error {
	return upsertOne(m, "default", "llamacpp-backends", []string{"10.10.1.160"}, 50053, "TCP", "biz-internet")
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
	go func() { done <- upsertChanged(m) }()
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

	if err := deleteOne(m, "default", "llamacpp-backends"); err != nil {
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
		if err := upsertOne(m, "default", name, []string{address}, 8080, "TCP", "biz-internet"); err != nil {
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
	if err := deleteOne(m, "default", "business-app-10"); err != nil {
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
// without activating it again, leaving the network-wide policy off. A change
// that fails before the deactivate, or after the activate, must not toggle it
// again.
func TestFailedChangeActivatesCentralPolicyAgain(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, fail string
		existing   bool
		delete     bool
		inWindow   bool
	}{
		{"upsert, new dataprefix list", "POST /template/policy/list/dataprefix/", false, false, false},
		{"upsert, existing dataprefix list", "PUT /template/policy/list/dataprefix/", true, false, true},
		{"upsert, approute", "PUT /template/policy/definition/approute/", false, false, true},
		{"delete, approute", "PUT /template/policy/definition/approute/", true, true, true},
		{"delete, dataprefix list", "DELETE /template/policy/list/dataprefix/", true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, ts := newFakeVManage(t)
			m := newTestManager(t, ts.URL)
			if tc.existing {
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
				err = deleteOne(m, "default", "llamacpp-backends")
			} else {
				err = upsertChanged(m)
			}
			if err == nil || !strings.Contains(err.Error(), "StatusCode 500") {
				t.Fatalf("expected the failed call's error, got %v", err)
			}
			_, calls := f.state()
			calls = calls[len(before):]
			failed := callIndex(calls, tc.fail)
			deactivate := callIndex(calls, "POST /template/policy/vsmart/deactivate/")
			activate := callIndex(calls, "POST /template/policy/vsmart/activate/")
			if failed < 0 || (deactivate < failed && failed < activate) != tc.inWindow {
				t.Fatalf("%q failed at %d, deactivate at %d, activate at %d; calls: %q", tc.fail, failed, deactivate, activate, calls)
			}
			if countCalls(calls, "POST /template/policy/vsmart/deactivate/cp-1") != countCalls(calls, "POST /template/policy/vsmart/activate/cp-1") {
				t.Fatalf("central policy left deactivated; calls: %q", calls)
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

	if err := deleteOne(m, "default", "unknown"); err != nil {
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

	if err := upsertOne(m, "default", "app", []string{"10.0.0.1"}, 8080, "TCP", "biz-internet mpls"); err != nil {
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

			err := upsertOne(m, "default", "app", []string{tc.address}, 8080, tc.protocol, tc.tunnel)
			if !errors.Is(err, ErrInvalidSpec) {
				t.Fatalf("expected ErrInvalidSpec, got %v", err)
			}
			if _, calls := f.state(); len(calls) != 0 {
				t.Fatalf("vManage called for an invalid spec: %q", calls)
			}
		})
	}
}

// Before names were hashed, the operator sent the full name, which vManage
// kept if it had at most 32 characters and otherwise shortened (see
// vManageStoredName). Those lists and their approute sequences must be removed
// once the service has a list under its new name, or when it is deleted.
func TestLegacyListsAreRemoved(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, service, legacy string
		delete                bool
	}{
		// the list vManage 20.15 holds for the testbed's llamacpp-backends
		{"upsert, name over 32 characters", "llamacpp-backends-TCP-50052", "default___llamacpp-ba-1140021977", false},
		{"upsert, name of 20 to 32 characters", "llamacpp-backends", "default___llamacpp-backends", false},
		{"upsert, name of exactly 20 characters", "business-a", "default___business-a", false},
		{"delete, name over 32 characters", "llamacpp-backends-TCP-50052", "default___llamacpp-ba-1140021977", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, ts := newFakeVManage(t)
			f.addLegacyList(t, tc.legacy, "dp-legacy")
			m := newTestManager(t, ts.URL)

			var err error
			if tc.delete {
				err = deleteOne(m, "default", tc.service)
			} else {
				err = upsertOne(m, "default", tc.service, []string{"10.0.0.1"}, 8080, "TCP", "biz-internet")
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

// Services whose long names share their first 21 characters had different
// lists in vManage (the hash differs): an upsert must remove only its own.
func TestLegacyListOfAnotherServiceIsKept(t *testing.T) {
	t.Parallel()
	f, ts := newFakeVManage(t)
	other := vManageStoredName("default___llamacpp-backends-TCP-50053")
	f.addLegacyList(t, other, "dp-other")
	f.addLegacyList(t, "default___llamacpp-ba-1140021977", "dp-legacy")
	m := newTestManager(t, ts.URL)

	if err := upsertOne(m, "default", "llamacpp-backends-TCP-50052", []string{"10.0.0.1"}, 50052, "TCP", "biz-internet"); err != nil {
		t.Fatal(err)
	}
	lists := f.lists()
	if _, ok := lists["default___llamacpp-ba-1140021977"]; ok || f.usesList("dp-legacy") {
		t.Fatalf("own legacy list not removed: lists %v", lists)
	}
	if _, ok := lists[other]; !ok || !f.usesList("dp-other") {
		t.Fatalf("another service's legacy list %q was touched: lists %v", other, lists)
	}
}

// A service with a short name never had a legacy list: another service's
// lists must be left alone.
func TestShortNamesHaveNoLegacyList(t *testing.T) {
	t.Parallel()
	f, ts := newFakeVManage(t)
	f.addLegacyList(t, "default___app-backend", "dp-other")
	m := newTestManager(t, ts.URL)

	if err := upsertOne(m, "default", "app", []string{"10.0.0.1"}, 8080, "TCP", "biz-internet"); err != nil {
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
		if err := upsertOne(m, "default", "llamacpp-backends", []string{}, 50052, "TCP", "biz-internet"); err != nil {
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

			if err := upsertOne(m, "default", "app", []string{"10.0.0.1"}, 8080, protocol, "biz-internet"); err != nil {
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

	if err := upsertOne(m, "default", "app", []string{"fd00::1", "10.0.0.1"}, 8080, "TCP", "biz-internet"); err != nil {
		t.Fatal(err)
	}
	id := f.lists()["default___app"]
	if id == "" || !f.usesList(id) {
		t.Fatalf("no list for the IPv4 endpoint: %v", f.lists())
	}

	// with only IPv6 endpoints left there is nothing to route
	if err := upsertOne(m, "default", "app", []string{"fd00::1"}, 8080, "TCP", "biz-internet"); err != nil {
		t.Fatal(err)
	}
	if lists := f.lists(); len(lists) != 0 || f.usesList(id) {
		t.Fatalf("list not removed without IPv4 endpoints: %v", lists)
	}
}

// The checks moved into sdwanManager, so a dry run accepted specs that a real
// run rejects.
func TestDryRunRejectsInvalidSpec(t *testing.T) {
	t.Parallel()
	m, err := NewManager(Config{DryRun: true}, logr.Discard())
	if err != nil {
		t.Fatal(err)
	}
	if err := upsertOne(m, "default", "app", []string{"10.0.0.1"}, 8080, "TCP", "biz-internet"); err != nil {
		t.Fatalf("dry run rejects a valid spec: %v", err)
	}
	if err := upsertOne(m, "default", "app", []string{"10.0.0.1"}, 8080, "TCP", "Biz-Internet"); !errors.Is(err, ErrInvalidSpec) {
		t.Fatalf("dry run accepts an invalid tunnel: %v", err)
	}
	if err := upsertOne(m, "default", "app", []string{"10.0.0.1"}, 8080, "ICMP", "biz-internet"); !errors.Is(err, ErrInvalidSpec) {
		t.Fatalf("dry run accepts an unknown protocol: %v", err)
	}
}

// Changes that cannot succeed are retried: they must not toggle the central
// policy, or fail for a leftover that does not matter.
func TestUpsertThatCannotSucceedLeavesCentralPolicyAlone(t *testing.T) {
	t.Parallel()
	f, ts := newFakeVManage(t)
	f.noApproute = true
	m := newTestManager(t, ts.URL)

	if err := upsert(m); err == nil {
		t.Fatal("expected an error without the approute definition")
	}
	if _, calls := f.state(); slices.ContainsFunc(calls, func(c string) bool { return strings.HasPrefix(c, "POST /template/policy/vsmart/") }) {
		t.Fatalf("central policy toggled for a change that cannot succeed; calls: %q", calls)
	}
}

func TestUpsertKeepsLegacyListVManageRefusesToDelete(t *testing.T) {
	t.Parallel()
	f, ts := newFakeVManage(t)
	f.addLegacyList(t, "default___llamacpp-backends", "dp-legacy")
	f.fail = "DELETE /template/policy/list/dataprefix/dp-legacy"
	m := newTestManager(t, ts.URL)

	if err := upsert(m); err != nil {
		t.Fatalf("upsert failed for the legacy list: %v", err)
	}
	if f.usesList("dp-legacy") {
		t.Fatalf("legacy list still routes: %s", f.approuteDefinition())
	}
}

// The cache is refreshed for every change, and each refresh logged all of it.
func TestRefreshCacheLogsCacheOnlyVerbose(t *testing.T) {
	t.Parallel()
	_, ts := newFakeVManage(t)
	m := newTestManager(t, ts.URL)
	var mu sync.Mutex
	var logs []string
	m.log = funcr.New(func(_, args string) {
		mu.Lock()
		defer mu.Unlock()
		logs = append(logs, args)
	}, funcr.Options{})

	if err := m.refreshCache(); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if slices.ContainsFunc(logs, func(l string) bool { return strings.Contains(l, "object cache: map") }) {
		t.Fatalf("full cache logged at the default verbosity: %q", logs)
	}
}

func appChange(name, address string) Change {
	return Change{Namespace: "default", Name: name, Endpoints: []string{address}, TargetPort: 8080, Protocol: "TCP", Tunnel: "biz-internet"}
}

func checkNoErrors(t *testing.T, errs []error) {
	t.Helper()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("change %d: %v", i, err)
		}
	}
}

// Every change deactivated and activated the central policy, so N changes
// took N cycles of ≈ 17 s on the testbed.
func TestApplyBatchInOneCycle(t *testing.T) {
	t.Parallel()
	f, ts := newFakeVManage(t)
	m := newTestManager(t, ts.URL)
	var batch []Change
	for i := range 5 {
		batch = append(batch, appChange(fmt.Sprintf("app-%d", i), fmt.Sprintf("10.0.0.%d", i+1)))
	}

	checkNoErrors(t, m.Apply(batch))
	_, calls := f.state()
	if d, a := countCalls(calls, "POST /template/policy/vsmart/deactivate/cp-1"), countCalls(calls, "POST /template/policy/vsmart/activate/cp-1"); d != 1 || a != 1 {
		t.Fatalf("%d deactivates and %d activates for a batch, want 1 each", d, a)
	}
	if n := countCalls(calls, "PUT /template/policy/definition/approute/ar-1"); n != 1 {
		t.Fatalf("%d approute PUTs for a batch, want 1", n)
	}
	lists := f.lists()
	seqs := gjson.Get(f.approuteDefinition(), "sequences").Array()
	if len(lists) != 5 || len(seqs) != 10 {
		t.Fatalf("%d lists and %d sequences, want 5 and 10: %s", len(lists), len(seqs), f.approuteDefinition())
	}
	ids := map[int64]bool{}
	for _, s := range seqs {
		ids[s.Get("sequenceId").Int()] = true
	}
	if len(ids) != len(seqs) {
		t.Fatalf("sequence ids not unique: %s", f.approuteDefinition())
	}
	for i := range 5 {
		if id := lists[convertK8sNameToSdWan("default", fmt.Sprintf("app-%d", i))]; !f.usesList(id) {
			t.Fatalf("app-%d has no sequences", i)
		}
	}

	// upserts of existing and new services, and deletes, in one batch
	_, before := f.state()
	batch = []Change{
		appChange("app-0", "10.0.1.1"),
		{Namespace: "default", Name: "app-1", Delete: true},
		appChange("app-new", "10.0.1.2"),
		{Namespace: "default", Name: "app-2", Endpoints: []string{}, TargetPort: 8080, Protocol: "TCP", Tunnel: "biz-internet"},
	}
	checkNoErrors(t, m.Apply(batch))
	_, calls = f.state()
	calls = calls[len(before):]
	if d, a := countCalls(calls, "POST /template/policy/vsmart/deactivate/cp-1"), countCalls(calls, "POST /template/policy/vsmart/activate/cp-1"); d != 1 || a != 1 {
		t.Fatalf("%d deactivates and %d activates for a mixed batch, want 1 each", d, a)
	}
	lists = f.lists()
	if len(lists) != 4 {
		t.Fatalf("lists after the mixed batch: %v", lists)
	}
	for _, name := range []string{"app-0", "app-3", "app-4", "app-new"} {
		if id := lists[convertK8sNameToSdWan("default", name)]; id == "" || !f.usesList(id) {
			t.Fatalf("%s has no list or sequences: lists %v, approute %s", name, lists, f.approuteDefinition())
		}
	}
	if n := len(gjson.Get(f.approuteDefinition(), "sequences").Array()); n != 8 {
		t.Fatalf("%d sequences after the mixed batch, want 8", n)
	}
}

// A change with an invalid spec fails on its own: the rest of the batch is
// applied.
func TestApplyBatchSkipsInvalidSpec(t *testing.T) {
	t.Parallel()
	f, ts := newFakeVManage(t)
	m := newTestManager(t, ts.URL)
	bad := appChange("bad", "10.0.0.2")
	bad.Tunnel = "Biz-Internet"

	errs := m.Apply([]Change{appChange("good", "10.0.0.1"), bad})
	if errs[0] != nil || !errors.Is(errs[1], ErrInvalidSpec) {
		t.Fatalf("errors %v, want nil and ErrInvalidSpec", errs)
	}
	lists := f.lists()
	if len(lists) != 1 || !f.usesList(lists["default___good"]) {
		t.Fatalf("valid change not applied: lists %v", lists)
	}
}

// An error that is not tied to one change fails every change of the batch.
func TestApplyBatchFailsTogether(t *testing.T) {
	t.Parallel()
	f, ts := newFakeVManage(t)
	f.fail = "PUT /template/policy/definition/approute/"
	m := newTestManager(t, ts.URL)

	errs := m.Apply([]Change{appChange("app-1", "10.0.0.1"), appChange("app-2", "10.0.0.2")})
	for i, err := range errs {
		if err == nil {
			t.Fatalf("change %d succeeded despite the failed approute PUT", i)
		}
	}
	if _, calls := f.state(); !slices.Contains(calls, "POST /template/policy/vsmart/activate/cp-1") {
		t.Fatalf("central policy not activated after the failed batch; calls: %q", calls)
	}
}

// callIndex returns the index of the first call starting with prefix, or -1.
func callIndex(calls []string, prefix string) int {
	return slices.IndexFunc(calls, func(c string) bool { return strings.HasPrefix(c, prefix) })
}

// The edges route annotated traffic without a policy between the deactivate
// and the activate: only the calls that need the policy off may happen in
// between. A new list is not used yet, and a removed one no longer is.
func TestApplyKeepsListCallsOutOfWindow(t *testing.T) {
	t.Parallel()
	f, ts := newFakeVManage(t)
	m := newTestManager(t, ts.URL)
	checkNoErrors(t, m.Apply([]Change{appChange("app-old", "10.0.0.1"), appChange("app-gone", "10.0.0.2")}))
	lists := f.lists()
	oldID, goneID := lists["default___app-old"], lists["default___app-gone"]
	_, before := f.state()

	checkNoErrors(t, m.Apply([]Change{
		appChange("app-old", "10.0.1.1"),
		appChange("app-new", "10.0.1.2"),
		{Namespace: "default", Name: "app-gone", Delete: true},
	}))
	_, calls := f.state()
	calls = calls[len(before):]
	deactivate := callIndex(calls, "POST /template/policy/vsmart/deactivate/")
	activate := callIndex(calls, "POST /template/policy/vsmart/activate/")
	for _, tc := range []struct {
		call     string
		inWindow bool
	}{
		{"POST /template/policy/list/dataprefix/", false},
		{"PUT /template/policy/list/dataprefix/" + oldID, true},
		{"PUT /template/policy/definition/approute/", true},
		{"DELETE /template/policy/list/dataprefix/" + goneID, false},
	} {
		i := callIndex(calls, tc.call)
		if i < 0 || (deactivate < i && i < activate) != tc.inWindow {
			t.Fatalf("%q called at %d, deactivate at %d, activate at %d; calls: %q", tc.call, i, deactivate, activate, calls)
		}
	}
}

// A list that cannot be deleted after its sequences are gone no longer routes:
// only its change fails, and the retry deletes it without a cycle.
func TestFailedListDeleteFailsOnlyItsChange(t *testing.T) {
	t.Parallel()
	f, ts := newFakeVManage(t)
	m := newTestManager(t, ts.URL)
	checkNoErrors(t, m.Apply([]Change{appChange("app-1", "10.0.0.1"), appChange("app-2", "10.0.0.2")}))
	id := f.lists()["default___app-1"]
	f.mu.Lock()
	f.fail = "DELETE /template/policy/list/dataprefix/" + id
	f.mu.Unlock()

	errs := m.Apply([]Change{{Namespace: "default", Name: "app-1", Delete: true}, appChange("app-2", "10.0.1.2")})
	if errs[0] == nil || errs[1] != nil {
		t.Fatalf("errors %v, want only the delete to fail", errs)
	}
	if f.usesList(id) {
		t.Fatalf("sequences of the deleted service left: %s", f.approuteDefinition())
	}

	f.mu.Lock()
	f.fail = ""
	f.mu.Unlock()
	_, before := f.state()
	checkNoErrors(t, m.Apply([]Change{{Namespace: "default", Name: "app-1", Delete: true}}))
	_, calls := f.state()
	calls = calls[len(before):]
	if callIndex(calls, "POST /template/policy/vsmart/") >= 0 || !slices.Contains(calls, "DELETE /template/policy/list/dataprefix/"+id) {
		t.Fatalf("retried delete toggled the central policy or kept the list; calls: %q", calls)
	}
}

// A list vManage rejects fails only its change: the rest of the batch goes
// ahead in the same cycle. A rejected update of an existing list keeps the
// service's old sequences, which still match the old list.
func TestRejectedListFailsOnlyItsChange(t *testing.T) {
	t.Parallel()
	for _, existing := range []bool{false, true} {
		t.Run(fmt.Sprintf("existing list %v", existing), func(t *testing.T) {
			t.Parallel()
			f, ts := newFakeVManage(t)
			m := newTestManager(t, ts.URL)
			if existing {
				checkNoErrors(t, m.Apply([]Change{appChange("app-1", "10.0.0.1"), appChange("app-2", "10.0.0.2")}))
			}
			f.mu.Lock()
			f.rejectList = "default___app-1"
			f.mu.Unlock()
			_, before := f.state()

			changed := appChange("app-1", "10.0.1.1")
			changed.TargetPort = 9090
			errs := m.Apply([]Change{changed, appChange("app-2", "10.0.1.2")})
			if errs[0] == nil || !strings.Contains(errs[0].Error(), "StatusCode 400") || errs[1] != nil {
				t.Fatalf("errors %v, want only the rejected list's change to fail", errs)
			}
			_, calls := f.state()
			calls = calls[len(before):]
			if d, a := countCalls(calls, "POST /template/policy/vsmart/deactivate/cp-1"), countCalls(calls, "POST /template/policy/vsmart/activate/cp-1"); d != 1 || a != 1 {
				t.Fatalf("%d deactivates and %d activates, want 1 each; calls: %q", d, a, calls)
			}
			def := f.approuteDefinition()
			lists := f.lists()
			if !f.usesList(lists["default___app-2"]) {
				t.Fatalf("app-2 not applied: %s", def)
			}
			id, ok := lists["default___app-1"]
			if !existing {
				if ok {
					t.Fatalf("rejected list created: %v", lists)
				}
				return
			}
			// the second match entry is the port (generateSeqRule)
			ports := gjson.Get(def, `sequences.#(match.entries.0.ref=="`+id+`")#.match.entries.1.value`).Array()
			if len(ports) != 2 || ports[0].String() != "8080" || ports[1].String() != "8080" {
				t.Fatalf("app-1 lost its old sequences: %s", def)
			}
		})
	}
}

// An error that would hit any call fails the batch before the central policy
// is toggled.
func TestListErrorFailsBatch(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusInternalServerError, http.StatusForbidden} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			t.Parallel()
			f, ts := newFakeVManage(t)
			f.fail = "POST /template/policy/list/dataprefix/"
			f.failStatus = status
			m := newTestManager(t, ts.URL)

			errs := m.Apply([]Change{appChange("app-1", "10.0.0.1"), appChange("app-2", "10.0.0.2")})
			if errs[0] == nil || errs[1] == nil {
				t.Fatalf("errors %v, want the batch to fail", errs)
			}
			if _, calls := f.state(); callIndex(calls, "POST /template/policy/vsmart/") >= 0 {
				t.Fatalf("central policy toggled for a failed batch; calls: %q", calls)
			}
			if _, calls := f.state(); countCalls(calls, "POST /template/policy/list/dataprefix/") != 1 {
				t.Fatalf("batch went on after the first failure; calls: %q", calls)
			}
		})
	}
}

func TestRejected(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{errors.New("HTTP Request failed: StatusCode 400"), true},
		{errors.New("HTTP Request failed: StatusCode 404"), true},
		{fmt.Errorf("upsert: %w", errors.New("HTTP Request failed: StatusCode 409")), true},
		// go-sdwan does not log in again on these: the session or the
		// token is gone for every call
		{errors.New("HTTP Request failed: StatusCode 401"), false},
		{errors.New("HTTP Request failed: StatusCode 403"), false},
		{errors.New("HTTP Request failed: StatusCode 408"), false},
		{errors.New("HTTP Request failed: StatusCode 429"), false},
		{errors.New("HTTP Request failed: StatusCode 500"), false},
		{errors.New("JSON error: {}"), false},
		{errInvalidResponse, false},
	} {
		if got := rejected(tc.err); got != tc.want {
			t.Errorf("rejected(%q) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

// Every upsert rewrote the sequences and toggled the central policy, even when
// nothing changed, e.g. for a resync or a retry queued before a newer change
// succeeded.
func TestUnchangedPolicyNeedsNoCycle(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                        string
		endpoints                   []string
		port                        int64
		noEntries                   bool
		cycle, listPut, approutePut bool
	}{
		{"unchanged", []string{"10.0.0.1", "10.0.0.2"}, 8080, false, false, false, false},
		{"endpoints in another order", []string{"10.0.0.2", "10.0.0.1"}, 8080, false, false, false, false},
		{"endpoints", []string{"10.0.0.1", "10.0.0.3"}, 8080, false, true, true, false},
		{"port", []string{"10.0.0.1", "10.0.0.2"}, 9090, false, true, false, true},
		// without the entries in the list of lists, the list may differ
		{"unchanged, entries not listed", []string{"10.0.0.1", "10.0.0.2"}, 8080, true, true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, ts := newFakeVManage(t)
			m := newTestManager(t, ts.URL)
			if err := upsertOne(m, "default", "app", []string{"10.0.0.1", "10.0.0.2"}, 8080, "TCP", "biz-internet"); err != nil {
				t.Fatal(err)
			}
			f.mu.Lock()
			f.noEntries = tc.noEntries
			f.mu.Unlock()
			_, before := f.state()

			if err := upsertOne(m, "default", "app", tc.endpoints, tc.port, "TCP", "biz-internet"); err != nil {
				t.Fatal(err)
			}
			_, calls := f.state()
			calls = calls[len(before):]
			id := f.lists()["default___app"]
			cycle := countCalls(calls, "POST /template/policy/vsmart/deactivate/cp-1") == 1 &&
				countCalls(calls, "POST /template/policy/vsmart/activate/cp-1") == 1
			listPut := slices.Contains(calls, "PUT /template/policy/list/dataprefix/"+id)
			approutePut := slices.Contains(calls, "PUT /template/policy/definition/approute/ar-1")
			if cycle != tc.cycle || listPut != tc.listPut || approutePut != tc.approutePut {
				t.Fatalf("cycle %v, list PUT %v, approute PUT %v; want %v, %v, %v; calls: %q",
					cycle, listPut, approutePut, tc.cycle, tc.listPut, tc.approutePut, calls)
			}
			if n := len(gjson.Get(f.approuteDefinition(), "sequences").Array()); n != 2 || !f.usesList(id) {
				t.Fatalf("sequences wrong after the upsert: %s", f.approuteDefinition())
			}
		})
	}
}

func TestSameSequences(t *testing.T) {
	t.Parallel()
	rule := func(id int, tunnel string) string {
		r, err := generateSeqRule(id, "source", "dp-1", "8080", "6", tunnel)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	dst, err := generateSeqRule(2, "destination", "dp-1", "8080", "6", "mpls")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		a, b []string
		want bool
	}{
		{"none", nil, nil, true},
		{"other ids and order", []string{rule(1, "mpls"), dst}, []string{dst, rule(7, "mpls")}, true},
		{"other tunnel", []string{rule(1, "mpls")}, []string{rule(1, "biz-internet")}, false},
		{"one more", []string{rule(1, "mpls")}, []string{rule(1, "mpls"), dst}, false},
		{"unparsable", []string{`{"sequenceId":"x"}`}, []string{`{"sequenceId":"x"}`}, false},
	} {
		if got := sameSequences(tc.a, tc.b); got != tc.want {
			t.Errorf("%s: sameSequences = %v, want %v", tc.name, got, tc.want)
		}
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

// newInPlaceManager returns a manager that edits the active central policy.
func newInPlaceManager(t *testing.T, url string) *sdwanManager {
	t.Helper()
	m := newTestManager(t, url)
	m.inPlace = true
	return m
}

// With in-place updates the central policy is never deactivated: lists and the
// approute definition are edited while it is active, and the vSmart template
// they affect is pushed once, after all the edits, with the device inputs
// marked as edited.
func TestInPlaceUpdateNeverDeactivates(t *testing.T) {
	t.Parallel()
	f, ts := newFakeVManage(t)
	m := newInPlaceManager(t, ts.URL)
	checkNoErrors(t, m.Apply([]Change{appChange("app-old", "10.0.0.1"), appChange("app-gone", "10.0.0.2")}))
	_, before := f.state()

	checkNoErrors(t, m.Apply([]Change{
		appChange("app-old", "10.0.1.1"),
		appChange("app-new", "10.0.1.2"),
		{Namespace: "default", Name: "app-gone", Delete: true},
	}))
	_, calls := f.state()
	if n := countCalls(calls, "POST /template/policy/vsmart/deactivate/cp-1"); n != 0 {
		t.Fatalf("%d deactivates with in-place updates; calls: %q", n, calls)
	}
	calls = calls[len(before):]
	if n := countCalls(calls, "POST /template/device/config/attachcli"); n != 1 {
		t.Fatalf("%d template pushes for a batch, want 1; calls: %q", n, calls)
	}
	push := callIndex(calls, "POST /template/device/config/attachcli")
	for _, call := range []string{"PUT /template/policy/list/dataprefix/", "PUT /template/policy/definition/approute/"} {
		if i := callIndex(calls, call); i < 0 || i > push {
			t.Fatalf("%q at %d, push at %d; calls: %q", call, i, push, calls)
		}
	}
	// a removed list is deleted only once the pushed policy no longer uses it
	if i := callIndex(calls, "DELETE /template/policy/list/dataprefix/"); i < push {
		t.Fatalf("list deleted at %d, before the push at %d; calls: %q", i, push, calls)
	}
	if callIndex(calls, "DELETE /template/lock/") >= 0 {
		t.Fatalf("edit session discarded; calls: %q", calls)
	}
	f.mu.Lock()
	attach := strings.SplitN(f.attaches[len(f.attaches)-1], " ", 2)[1]
	f.mu.Unlock()
	for _, path := range []string{"deviceTemplateList.0.templateId", "deviceTemplateList.0.device.0.csv-templateId"} {
		if v := gjson.Get(attach, path).String(); v != "tmpl-1" {
			t.Fatalf("%s = %q in %s", path, v, attach)
		}
	}
	if !gjson.Get(attach, "deviceTemplateList.0.isEdited").Bool() || gjson.Get(attach, "deviceTemplateList.0.device.0.csv-deviceId").String() != "vsmart-1" {
		t.Fatalf("push without the edited device inputs: %s", attach)
	}
	if !strings.Contains(f.approuteDefinition(), `"ref":"`+f.lists()["default___app-new"]+`"`) {
		t.Fatalf("approute without the new list: %s", f.approuteDefinition())
	}
}

// The first in-place attempt reactivated the policy and then deleted the edit
// session's lock, which rolled vManage's definition back: the next change was
// built on the old definition. Flips in a row must each leave the new state in
// vManage and end the session.
func TestInPlaceUpdateFlipsInARow(t *testing.T) {
	t.Parallel()
	f, ts := newFakeVManage(t)
	m := newInPlaceManager(t, ts.URL)
	checkNoErrors(t, m.Apply([]Change{appChange("app-1", "10.0.0.1")}))
	for i, tunnel := range []string{"public-internet", "biz-internet", "public-internet", "biz-internet"} {
		c := appChange("app-1", "10.0.0.1")
		c.Tunnel = tunnel
		checkNoErrors(t, m.Apply([]Change{c}))
		if !strings.Contains(f.approuteDefinition(), `"parameter":"`+tunnel+`"`) {
			t.Fatalf("flip %d to %s not in vManage's definition: %s", i, tunnel, f.approuteDefinition())
		}
		f.mu.Lock()
		lock := f.lock
		f.mu.Unlock()
		if lock != "" {
			t.Fatalf("flip %d left the edit session %s open", i, lock)
		}
	}
	_, calls := f.state()
	if n := countCalls(calls, "POST /template/policy/vsmart/deactivate/cp-1"); n != 0 {
		t.Fatalf("%d deactivates; calls: %q", n, calls)
	}
}

// The testbed's vSmart template is a CLI template, which vManage's UI pushes
// with attachcli; pushing it with attachfeature made the vSmart answer
// access-denied, and vManage rolled the edit back. A feature template is
// pushed with attachfeature.
func TestInPlaceUpdatePushesTemplateByType(t *testing.T) {
	t.Parallel()
	for _, feature := range []bool{false, true} {
		f, ts := newFakeVManage(t)
		m := newInPlaceManager(t, ts.URL)
		f.mu.Lock()
		f.featureTemplate = feature
		f.mu.Unlock()
		checkNoErrors(t, m.Apply([]Change{appChange("app-1", "10.0.0.1")}))
		c := appChange("app-1", "10.0.0.1")
		c.Tunnel = "public-internet"
		checkNoErrors(t, m.Apply([]Change{c}))
		want := map[bool]string{false: "attachcli", true: "attachfeature"}[feature]
		_, calls := f.state()
		if n := countCalls(calls, "POST /template/device/config/"+want); n != 2 {
			t.Fatalf("feature template %v: %d %s pushes, want 2; calls: %q", feature, n, want, calls)
		}
		if !strings.Contains(f.approuteDefinition(), `"parameter":"public-internet"`) {
			t.Fatalf("feature template %v: edit not kept: %s", feature, f.approuteDefinition())
		}
	}
}

// A push the vSmart refuses fails the batch with its reason, and vManage's
// rollback is not taken for success.
func TestInPlaceUpdateReportsRefusedPush(t *testing.T) {
	t.Parallel()
	f, ts := newFakeVManage(t)
	m := newInPlaceManager(t, ts.URL)
	f.mu.Lock()
	f.attachFails = true
	f.mu.Unlock()
	err := m.Apply([]Change{appChange("app-1", "10.0.0.1")})[0]
	if err == nil || !strings.Contains(err.Error(), "access-denied") {
		t.Fatalf("error %v, want the vSmart's reason", err)
	}
}

// Without affected templates (a vSmart policy not pushed by a template) the
// edit is applied by reactivating the policy.
func TestInPlaceUpdateReactivatesWithoutTemplates(t *testing.T) {
	t.Parallel()
	f, ts := newFakeVManage(t)
	m := newInPlaceManager(t, ts.URL)
	f.mu.Lock()
	f.noTemplates = true
	f.mu.Unlock()
	checkNoErrors(t, m.Apply([]Change{appChange("app-1", "10.0.0.1")}))
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.attaches) != 0 || len(f.policyCalls) == 0 {
		t.Fatalf("attaches %q, policy calls %q", f.attaches, f.policyCalls)
	}
	if last := f.policyCalls[len(f.policyCalls)-1]; last != `/template/policy/vsmart/activate/cp-1?confirm=true {"isEdited":true}` {
		t.Fatalf("push %q", last)
	}
}

// The edits a failed change made are in vManage: they must be pushed, so that
// vManage and the vSmarts do not disagree, and the change still fails.
func TestInPlaceUpdatePushesAfterFailedChange(t *testing.T) {
	t.Parallel()
	f, ts := newFakeVManage(t)
	m := newInPlaceManager(t, ts.URL)
	checkNoErrors(t, m.Apply([]Change{appChange("app-1", "10.0.0.1")}))
	f.mu.Lock()
	f.fail = "PUT /template/policy/definition/approute/"
	f.mu.Unlock()
	_, before := f.state()

	// new endpoints and a new port: the list PUT succeeds, the approute PUT fails
	c := appChange("app-1", "10.0.0.9")
	c.TargetPort = 9090
	if err := m.Apply([]Change{c})[0]; err == nil {
		t.Fatal("change succeeded although the approute PUT failed")
	}
	_, calls := f.state()
	calls = calls[len(before):]
	if callIndex(calls, "PUT /template/policy/list/dataprefix/") < 0 ||
		countCalls(calls, "POST /template/device/config/attachcli") != 1 {
		t.Fatalf("edited list not pushed after the failed change; calls: %q", calls)
	}
}

// A push that fails on the vSmart fails the batch, and is retried like any
// other failure.
func TestInPlaceUpdateFailedPushFailsBatch(t *testing.T) {
	t.Parallel()
	f, ts := newFakeVManage(t)
	m := newInPlaceManager(t, ts.URL)
	f.mu.Lock()
	f.attachFails = true
	f.mu.Unlock()
	errs := m.Apply([]Change{appChange("app-1", "10.0.0.1"), appChange("app-2", "10.0.0.2")})
	if errs[0] == nil || errs[1] == nil {
		t.Fatalf("errors %v, want both changes to fail when the push fails", errs)
	}
}

// A batch that changes nothing in use needs no push, as it needs no cycle.
func TestInPlaceUpdateSkipsPushWithoutChanges(t *testing.T) {
	t.Parallel()
	f, ts := newFakeVManage(t)
	m := newInPlaceManager(t, ts.URL)
	checkNoErrors(t, m.Apply([]Change{appChange("app-1", "10.0.0.1")}))
	_, before := f.state()
	checkNoErrors(t, m.Apply([]Change{appChange("app-1", "10.0.0.1")}))
	_, calls := f.state()
	calls = calls[len(before):]
	if callIndex(calls, "POST /template/device/config/attach") >= 0 || callIndex(calls, "POST /template/policy/vsmart/") >= 0 {
		t.Fatalf("push for an unchanged policy; calls: %q", calls)
	}
}

// If the push does not end the edit session, the next change is made with a
// deactivate/activate cycle, which ends it, instead of failing until it gives
// up.
func TestInPlaceUpdateFallsBackToCycleWhenLocked(t *testing.T) {
	t.Parallel()
	f, ts := newFakeVManage(t)
	m := newInPlaceManager(t, ts.URL)
	f.mu.Lock()
	f.sticky = true
	f.mu.Unlock()
	// the first change's approute PUT opens a session the push does not end
	checkNoErrors(t, m.Apply([]Change{appChange("app-1", "10.0.0.1")}))
	f.mu.Lock()
	f.pushed = true // as after vManage's push: further PUTs are refused
	f.mu.Unlock()
	_, before := f.state()
	c := appChange("app-1", "10.0.0.1")
	c.Tunnel = "public-internet"
	checkNoErrors(t, m.Apply([]Change{c})) // locked: falls back to a cycle
	_, calls := f.state()
	calls = calls[len(before):]
	if countCalls(calls, "POST /template/policy/vsmart/deactivate/cp-1") != 1 || countCalls(calls, "POST /template/policy/vsmart/activate/cp-1") != 1 {
		t.Fatalf("want one cycle for the locked policy; calls: %q", calls)
	}
	if !strings.Contains(f.approuteDefinition(), `"parameter":"public-internet"`) {
		t.Fatalf("change not applied: %s", f.approuteDefinition())
	}
}

func TestLockedIsNotRejected(t *testing.T) {
	t.Parallel()
	err := lockedErr(gjson.Parse(`{"error":{"code":"POLICY0013","details":"locked"}}`), errors.New("HTTP Request failed: StatusCode 400"))
	if !errors.Is(err, errPolicyLocked) || rejected(err) {
		t.Fatalf("locked error %v: errPolicyLocked %v, rejected %v", err, errors.Is(err, errPolicyLocked), rejected(err))
	}
}
