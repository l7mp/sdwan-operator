package sdwan

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/netip"
	"regexp"
	"slices"
	"strconv"
	"time"

	"github.com/go-logr/logr"
	"github.com/netascode/go-sdwan"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	CentralizedPolicyName = "SDWANOperatorCentralizedPolicy"
	ApproutePolicyName    = "SDWANOperatorApproutePolicy"
	defaultDescription    = "managed by SD-WAN operator"

	tasksEndpoint           = "/device/action/status/tasks"
	defaultTaskWaitTimeout  = 5 * time.Minute
	defaultTaskPollInterval = 100 * time.Millisecond
)

// errInvalidResponse is returned when vManage answers with something other
// than the expected JSON. After vManage ends a session (e.g. idle timeout),
// every request gets HTTP 200 with the HTML login page, which go-sdwan
// reports as success with an empty body.
var errInvalidResponse = errors.New("unexpected vManage response (session expired?)")

// statusRe matches the HTTP status in the errors of go-sdwan, which has no
// error type for it.
var statusRe = regexp.MustCompile(`HTTP Request failed: StatusCode (\d{3})`)

// rejected tells if vManage refused a call for what it was asked to do: a 4xx
// status other than an authentication failure, a timeout or a rate limit. Any
// other error, like an expired session or a 5xx status, would fail the other
// calls too.
func rejected(err error) bool {
	m := statusRe.FindStringSubmatch(err.Error())
	if m == nil {
		return false
	}
	code, _ := strconv.Atoi(m[1])
	switch code {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusRequestTimeout, http.StatusTooManyRequests:
		return false
	}
	return code >= 400 && code < 500
}

// ErrInvalidSpec is returned for a tunnel policy that cannot be applied as
// specified, so retrying it does not help.
var ErrInvalidSpec = errors.New("invalid tunnel policy")

// tunnelRe matches one or more vManage colors (e.g. "biz-internet mpls"), the
// parameter of the backupSlaPreferredColor action.
var tunnelRe = regexp.MustCompile(`^[a-z0-9-]+( [a-z0-9-]+)*$`)

// protocolNumbers maps the protocols of a Kubernetes Service to IP protocol
// numbers.
var protocolNumbers = map[string]int32{
	"TCP":  6,
	"UDP":  17,
	"SCTP": 132,
}

// Change is the SD-WAN state a Kubernetes object asks for: its endpoints
// routed over the tunnel, or, if Delete is set, nothing.
type Change struct {
	Namespace, Name string
	Delete          bool
	Endpoints       []string
	TargetPort      int64
	Protocol        string
	Tunnel          string
}

type Manager interface {
	// Apply makes the changes in one deactivate/activate cycle of the
	// central policy, at most one change per object. It returns the error of
	// each change, in order: nil for a change that was made.
	Apply(changes []Change) []error
}

func NewManager(c Config, log logr.Logger) (Manager, error) {
	if c.DryRun {
		return &nullManager{}, nil
	}

	// connect to vManage
	sc, err := sdwan.NewClient(c.URL, c.Insecure, sdwan.WithLogin(c.User, c.Password))
	if err != nil {
		return nil, err
	}

	m := &sdwanManager{
		client:           sc,
		objectCache:      make(map[string]map[string]string),
		log:              log,
		taskWaitTimeout:  defaultTaskWaitTimeout,
		taskPollInterval: defaultTaskPollInterval,
	}

	//populate SDWAN cache
	err = m.refreshCache()
	if err != nil {
		return nil, err
	}

	return m, nil
}

// nullManager changes nothing, but rejects what sdwanManager rejects, so that
// a dry run finds invalid specs.
type nullManager struct{}

func (m *nullManager) Apply(changes []Change) []error {
	errs := make([]error, len(changes))
	for i, c := range changes {
		if !c.Delete {
			_, _, errs[i] = checkUpsert(c.Endpoints, c.Protocol, c.Tunnel)
		}
	}
	return errs
}

type sdwanManager struct {
	client      sdwan.Client
	objectCache map[string]map[string]string
	log         logr.Logger
	// taskWaitTimeout bounds waitTasksFinish; taskPollInterval is its poll period.
	taskWaitTimeout  time.Duration
	taskPollInterval time.Duration
}

// refreshCache reloads the IDs of the vManage objects the operator uses, so
// that changes made in vManage since the last event, like a list deleted or
// recreated by hand, are seen. The cache is replaced only if every list was
// read: a response without a data array (e.g. the login page of an expired
// session) is errInvalidResponse.
func (m *sdwanManager) refreshCache() error {
	cache := make(map[string]map[string]string)
	for _, l := range []struct{ kind, endpoint, nameField, idField string }{
		{"central", "/template/policy/vsmart", "policyName", "policyId"},
		{"dataprefix", "/template/policy/list/dataprefix", "name", "listId"},
		{"approute", "/template/policy/definition/approute", "name", "definitionId"},
	} {
		res, err := m.client.Get(l.endpoint)
		if err != nil {
			return err
		}
		data := res.Get("data")
		if !data.IsArray() {
			return fmt.Errorf("GET %s: %w", l.endpoint, errInvalidResponse)
		}
		cache[l.kind] = make(map[string]string)
		for _, val := range data.Array() {
			cache[l.kind][val.Get(l.nameField).String()] = val.Get(l.idField).String()
		}
	}
	m.objectCache = cache

	// refreshed for every change: log the full cache only when asked to
	m.log.Info("object cache refreshed", "vsmart", len(cache["central"]),
		"dataprefix", len(cache["dataprefix"]), "approute", len(cache["approute"]))
	m.log.V(1).Info(fmt.Sprintf("object cache: %#v", m.objectCache))

	return nil
}

// update is a change that passed the checks, with what vManage needs for it.
type update struct {
	// idx is the index of the change in the batch.
	idx             int
	namespace, name string
	objName         string
	// listData is the dataprefix list to upsert, "" to delete the list.
	listData            string
	port, proto, tunnel string
	// listID and legacyID are the ids of the object's list and of its list
	// from before names were hashed, "" if it has none.
	listID, legacyID string
	// created tells if the list was created for this change.
	created bool
}

func (u *update) delete() bool { return u.listData == "" }

// prepare checks a change and returns the update that makes it.
func (m *sdwanManager) prepare(c Change) (*update, error) {
	u := &update{namespace: c.Namespace, name: c.Name, objName: convertK8sNameToSdWan(c.Namespace, c.Name)}
	if c.Delete {
		return u, nil
	}

	ipv4, protoNum, err := checkUpsert(c.Endpoints, c.Protocol, c.Tunnel)
	if err != nil {
		return nil, err
	}
	if len(ipv4) < len(c.Endpoints) {
		m.log.Info("skipping IPv6 endpoints: approute sequences match IPv4 only", "name", u.objName)
	}
	// Without endpoints there is nothing to route: remove the service's list
	// rather than sending vManage an empty one.
	if len(ipv4) == 0 {
		return u, nil
	}

	if u.listData, err = m.generateDataPrefixList(u.objName, ipv4); err != nil {
		return nil, err
	}
	u.port = strconv.Itoa(int(c.TargetPort))
	u.proto = strconv.Itoa(int(protoNum))
	u.tunnel = c.Tunnel
	return u, nil
}

func (m *sdwanManager) Apply(changes []Change) []error {
	errs := make([]error, len(changes))
	var updates []*update
	for i, c := range changes {
		u, err := m.prepare(c)
		if err != nil {
			errs[i] = err
			continue
		}
		u.idx = i
		updates = append(updates, u)
	}
	if len(updates) == 0 {
		return errs
	}
	fail := func(err error, us []*update) []error {
		for _, u := range us {
			errs[u.idx] = err
		}
		return errs
	}

	if err := m.ensureSession(); err != nil {
		return fail(err, updates)
	}

	if err := m.refreshCache(); err != nil {
		return fail(err, updates)
	}

	// a change that cannot succeed must not toggle the central policy
	approuteID, ok := m.objectCache["approute"][ApproutePolicyName]
	if !ok {
		return fail(errors.New("fail to get approute"), updates)
	}

	var changed []*update
	for _, u := range updates {
		u.listID = m.objectCache["dataprefix"][u.objName]
		u.legacyID = m.legacyListID(u.namespace, u.name)
		if u.delete() && u.listID == "" && u.legacyID == "" {
			m.log.Info("no dataprefix list to delete", "name", u.objName)
			continue
		}
		changed = append(changed, u)
	}
	if len(changed) == 0 {
		return errs
	}

	// No sequence uses a new list yet: create it before the central policy
	// is deactivated, to keep the time without a policy short. A list vManage
	// rejects fails only its change.
	kept := changed[:0:0]
	for _, u := range changed {
		if !u.delete() && u.listID == "" {
			if err := m.upsertDataPrefixList(u.objName, u.listData); err != nil {
				if !rejected(err) {
					return fail(err, changed)
				}
				errs[u.idx] = err
				continue
			}
			u.listID = m.objectCache["dataprefix"][u.objName]
			u.created = true
		}
		kept = append(kept, u)
	}
	changed = kept
	if len(changed) == 0 {
		return errs
	}

	endpoint := "/template/policy/definition/approute/" + approuteID
	def, err := m.approuteDefinition(endpoint)
	if err != nil {
		return fail(err, changed)
	}
	newDef, modified, err := replaceSequences(def, changed)
	if err != nil {
		return fail(err, changed)
	}

	// Deletes whose lists no sequence uses, e.g. retries of deletes that
	// failed after the sequences were removed, need no cycle.
	if modified {
		err := m.applyChange(fmt.Sprintf("%d change(s)", len(changed)), func() error {
			// A change whose list vManage rejects keeps its old list and
			// sequences: new sequences over the old list would pair a new
			// port, protocol or tunnel with the old endpoints.
			kept := changed[:0:0]
			for _, u := range changed {
				if !u.delete() && !u.created {
					if err := m.upsertDataPrefixList(u.objName, u.listData); err != nil {
						if !rejected(err) {
							return err
						}
						errs[u.idx] = err
						continue
					}
				}
				kept = append(kept, u)
			}
			if len(kept) < len(changed) {
				changed = kept
				var err error
				if newDef, modified, err = replaceSequences(def, changed); err != nil || !modified {
					return err
				}
			}
			return m.putApproute(endpoint, newDef)
		})
		if err != nil {
			return fail(err, changed)
		}
	}

	// No sequence uses the removed lists now: delete them while the central
	// policy is active. A failure fails only its change, whose retry needs no
	// cycle.
	for _, u := range changed {
		if u.delete() && u.listID != "" {
			if err := m.deleteDataPrefixList(u.listID); err != nil {
				errs[u.idx] = err
				continue
			}
		}
		if u.legacyID == "" {
			continue
		}
		// The list from before names were hashed is unused now. If vManage
		// keeps it (e.g. another policy uses it), it routes nothing for us,
		// so failing, and retrying, an upsert would not help.
		if err := m.deleteDataPrefixList(u.legacyID); err != nil {
			if u.delete() {
				errs[u.idx] = err
				continue
			}
			m.log.Error(err, "cannot delete legacy dataprefix list, leaving it", "id", u.legacyID)
		}
	}

	return errs
}

// legacyListID returns the id of the list a Kubernetes object has in vManage
// from before names were hashed, or "" if it has none.
func (m *sdwanManager) legacyListID(namespace, name string) string {
	for _, stored := range slices.Sorted(maps.Keys(m.objectCache["dataprefix"])) {
		if isLegacyListName(stored, namespace, name) {
			return m.objectCache["dataprefix"][stored]
		}
	}
	return ""
}

func (m *sdwanManager) deleteDataPrefixList(id string) error {
	endpoint := "/template/policy/list/dataprefix/" + id
	res, err := m.client.Delete(endpoint)
	if err != nil {
		return err
	}
	m.log.Info(fmt.Sprintf("DELETE %s: %s", endpoint, res))
	return nil
}

// applyChange makes a change while the central policy is deactivated. The
// policy is activated again even if the change fails, so that a failed change
// cannot leave the network-wide policy off.
func (m *sdwanManager) applyChange(op string, change func() error) error {
	if err := m.deactivateCentralPolicy(); err != nil {
		m.log.Error(err, "cannot deactivate central policy, continuing", "op", op)
	}

	err := change()
	if aerr := m.activateCentralPolicy(); aerr != nil {
		return errors.Join(err, aerr)
	}

	return err
}

func (m *sdwanManager) upsertDataPrefixList(objName, data string) error {
	endpoint := "/template/policy/list/dataprefix/"
	id, ok := m.objectCache["dataprefix"][objName]
	if ok {
		putEndpoint := endpoint + id
		res, err := m.client.Put(putEndpoint, data)
		if err != nil {
			m.log.Error(err, fmt.Sprintf("PUT %s: %s, result: %s", putEndpoint, data, res))
			return err
		}
		m.log.Info(fmt.Sprintf("PUT %s: %s, result: %s", putEndpoint, data, res))
	} else {
		res, err := m.client.Post(endpoint, data)
		if err != nil {
			m.log.Error(err, fmt.Sprintf("POST %s: %s, result: %s", endpoint, data, res))
			return err
		}
		if res.Get("listId").String() == "" {
			return fmt.Errorf("POST %s: %w", endpoint, errInvalidResponse)
		}
		m.objectCache["dataprefix"][objName] = res.Get("listId").String()
		m.log.Info(fmt.Sprintf("POST %s: %s, result: %s", endpoint, data, res))
	}

	return nil
}

// approuteDefinition returns the approute definition at endpoint, with a
// sequences array.
func (m *sdwanManager) approuteDefinition(endpoint string) (string, error) {
	desc, err := m.client.Get(endpoint)
	if err != nil {
		return "", err
	}
	// Writing back anything but the definition would replace it.
	if !desc.Get("name").Exists() {
		return "", fmt.Errorf("GET %s: %w", endpoint, errInvalidResponse)
	}
	data := desc.String()
	if !desc.Get("sequences").IsArray() {
		if data, err = sjson.SetRaw(data, "sequences", "[]"); err != nil {
			return "", err
		}
	}
	return data, nil
}

// replaceSequences returns the approute definition def with the sequences of
// the updates replaced: those of their lists and legacy lists removed, and
// those of the upserts added. It also tells if the definition changed.
func replaceSequences(def string, updates []*update) (string, bool, error) {
	var stale []string
	for _, u := range updates {
		for _, l := range []string{u.listID, u.legacyID} {
			if l != "" {
				stale = append(stale, l)
			}
		}
	}
	data := def
	for _, dpid := range stale {
		s := gjson.Get(data, "sequences.#(match.entries.0.ref!=\""+dpid+"\")#")
		var err error
		if data, err = sjson.SetRaw(data, "sequences", s.String()); err != nil {
			return "", false, err
		}
	}
	modified := len(gjson.Get(data, "sequences").Array()) < len(gjson.Get(def, "sequences").Array())

	// get sequence ID
	s := gjson.Get(data, "sequences.#.sequenceId")
	sids := make([]int64, 0, len(s.Array()))
	for _, v := range s.Array() {
		sids = append(sids, v.Int())
	}
	sid := 1
	if len(sids) > 0 {
		sid = int(slices.Max(sids) + 1)
	}

	// install new seqrules
	for _, u := range updates {
		if u.delete() {
			continue
		}
		for _, direction := range []string{"source", "destination"} {
			rule, err := generateSeqRule(sid, direction, u.listID, u.port, u.proto, u.tunnel)
			if err != nil {
				return "", false, err
			}
			if data, err = sjson.SetRaw(data, "sequences.-1", rule); err != nil {
				return "", false, err
			}
			sid++
			modified = true
		}
	}

	return data, modified, nil
}

func (m *sdwanManager) putApproute(endpoint, data string) error {
	res, err := m.client.Put(endpoint, data)
	if err != nil {
		m.log.Error(err, fmt.Sprintf("PUT %s: %s, result: %s", endpoint, data, res))
		return err
	}
	m.log.Info(fmt.Sprintf("PUT %s: %s, result: %s", endpoint, data, res))
	return nil
}

type matchEntry struct {
	Field string `json:"field"`
	Ref   string `json:"ref,omitempty"`
	Value string `json:"value,omitempty"`
}

type seqAction struct {
	Type      string `json:"type"`
	Parameter string `json:"parameter"`
}

type seqRule struct {
	SequenceID     int    `json:"sequenceId"`
	SequenceName   string `json:"sequenceName"`
	SequenceType   string `json:"sequenceType"`
	SequenceIPType string `json:"sequenceIpType"`
	Match          struct {
		Entries []matchEntry `json:"entries"`
	} `json:"match"`
	Actions []seqAction `json:"actions"`
}

// generateSeqRule returns the approute sequence that sends the traffic from
// (direction "source") or to (direction "destination") the endpoints of a
// dataprefix list over the tunnel.
func generateSeqRule(id int, direction, dataPrefixList, port, proto, tunnel string) (string, error) {
	r := seqRule{
		SequenceID:     id,
		SequenceName:   "App Route",
		SequenceType:   "appRoute",
		SequenceIPType: "ipv4",
	}
	r.Match.Entries = []matchEntry{
		{Field: direction + "DataPrefixList", Ref: dataPrefixList},
		{Field: direction + "Port", Value: port},
		{Field: "protocol", Value: proto},
	}
	r.Actions = []seqAction{{Type: "backupSlaPreferredColor", Parameter: tunnel}}
	b, err := json.Marshal(r)
	return string(b), err
}

type ipPrefix struct {
	IPPrefix string `json:"ipPrefix"`
}

type prefixList struct {
	Name        string     `json:"name"`
	Description string     `json:"description"`
	Type        string     `json:"type"`
	Entries     []ipPrefix `json:"entries"`
}

// checkUpsert checks the fields of an upsert. It returns the IPv4 endpoints
// and the IP protocol number.
// The approute sequences match IPv4 only, so the IPv6 endpoints of a
// dual-stack service are left out. Without IPv4 endpoints the upsert removes
// the service's list, so the other fields are not checked then.
func checkUpsert(endpoints []string, protocol, tunnel string) ([]string, int32, error) {
	ipv4 := make([]string, 0, len(endpoints))
	for _, val := range endpoints {
		addr, err := netip.ParseAddr(val)
		if err != nil {
			return nil, 0, fmt.Errorf("%w: endpoint %q is not an IP address", ErrInvalidSpec, val)
		}
		if addr.Is4() {
			ipv4 = append(ipv4, addr.String())
		}
	}
	if len(ipv4) == 0 {
		return ipv4, 0, nil
	}

	if !tunnelRe.MatchString(tunnel) {
		return nil, 0, fmt.Errorf("%w: tunnel %q is not a list of vManage colors", ErrInvalidSpec, tunnel)
	}
	protoNum, ok := protocolNumbers[protocol]
	if !ok {
		return nil, 0, fmt.Errorf("%w: unknown protocol %q", ErrInvalidSpec, protocol)
	}

	return ipv4, protoNum, nil
}

// generateDataPrefixList returns the dataprefix list of IPv4 endpoints.
func (m *sdwanManager) generateDataPrefixList(name string, endpoints []string) (string, error) {
	l := prefixList{
		Name:        name,
		Description: defaultDescription,
		Type:        "dataPrefix",
		Entries:     make([]ipPrefix, 0, len(endpoints)),
	}
	for _, val := range endpoints {
		l.Entries = append(l.Entries, ipPrefix{IPPrefix: val + "/32"})
	}
	b, err := json.Marshal(l)
	return string(b), err
}

func (m *sdwanManager) deactivateCentralPolicy() error {
	if err := m.toggleCentralPolicy(false); err != nil {
		return err
	}
	return m.waitTasksFinish()
}

func (m *sdwanManager) activateCentralPolicy() error {
	if err := m.toggleCentralPolicy(true); err != nil {
		return err
	}
	return m.waitTasksFinish()
}

func (m *sdwanManager) toggleCentralPolicy(activate bool) error {
	id, ok := m.objectCache["central"][CentralizedPolicyName]
	if !ok {
		return errors.New("fail to get centralized policy name")
	}

	baseUrl := "/template/policy/vsmart/activate/"
	if !activate {
		baseUrl = "/template/policy/vsmart/deactivate/"
	}

	endpoint := baseUrl + id
	var data string
	if activate {
		data = "{}"
	}
	res, err := m.client.Post(endpoint, data)
	if err != nil {
		return err
	}
	m.log.Info(fmt.Sprintf("POST %s: %s, result: %s", endpoint, data, res))
	// vManage starts a task and returns its id; anything else did not apply.
	if !res.Get("id").Exists() {
		return fmt.Errorf("POST %s: %w", endpoint, errInvalidResponse)
	}

	return nil
}

// runningTasks returns the vManage tasks that are still running. A response
// without a runningTasks list (e.g. the login page of an expired session) is
// errInvalidResponse.
func (m *sdwanManager) runningTasks() ([]gjson.Result, error) {
	res, err := m.client.Get(tasksEndpoint)
	if err != nil {
		return nil, err
	}
	tasks := res.Get("runningTasks")
	if !tasks.IsArray() {
		return nil, fmt.Errorf("GET %s: %w", tasksEndpoint, errInvalidResponse)
	}
	return tasks.Array(), nil
}

// ensureSession checks the vManage session before a change. go-sdwan logs in
// only while it has no token, so after vManage ends the session the token is
// cleared to force a new login on the next request.
func (m *sdwanManager) ensureSession() error {
	_, err := m.runningTasks()
	if err == nil {
		return nil
	}
	m.log.Info(fmt.Sprintf("vManage session check failed, logging in again: %s", err))
	m.client.Token = ""
	if _, err := m.runningTasks(); err != nil {
		return fmt.Errorf("vManage session: %w", err)
	}
	return nil
}

// waitTasksFinish waits until vManage has no running tasks, for at most
// taskWaitTimeout. An invalid response clears the token so that the next poll
// logs in again.
func (m *sdwanManager) waitTasksFinish() error {
	deadline := time.Now().Add(m.taskWaitTimeout)
	for {
		tasks, err := m.runningTasks()
		if err == nil && len(tasks) == 0 {
			return nil
		}
		if err != nil {
			m.log.Error(err, fmt.Sprintf("GET %s", tasksEndpoint))
			if errors.Is(err, errInvalidResponse) {
				m.client.Token = ""
			}
		}
		if time.Now().After(deadline) {
			if err == nil {
				err = fmt.Errorf("%d task(s) still running", len(tasks))
			}
			return fmt.Errorf("vManage tasks did not finish within %s: %w", m.taskWaitTimeout, err)
		}
		time.Sleep(m.taskPollInterval)
	}
}
