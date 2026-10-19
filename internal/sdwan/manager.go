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

// errPolicyLocked is returned when vManage refuses an edit because the
// policy is locked in edit mode (POLICY0013). An in-place edit of an active
// policy opens such a session; a deactivate ends it.
var errPolicyLocked = errors.New("policy locked in edit mode")

// policyLockedCode is vManage's error code for errPolicyLocked.
const policyLockedCode = "POLICY0013"

// statusRe matches the HTTP status in the errors of go-sdwan, which has no
// error type for it.
var statusRe = regexp.MustCompile(`HTTP Request failed: StatusCode (\d{3})`)

// rejected tells if vManage refused a call for what it was asked to do: a 4xx
// status other than an authentication failure, a timeout or a rate limit. Any
// other error, like an expired session or a 5xx status, would fail the other
// calls too.
func rejected(err error) bool {
	if errors.Is(err, errPolicyLocked) {
		return false
	}
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
	// Apply makes the changes in one update of the central policy (a
	// deactivate/activate cycle, or an in-place edit and push), at most one
	// change per object. It returns the error of each change, in order: nil
	// for a change that was made.
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
		inPlace:          c.InPlaceUpdate,
	}

	if m.inPlace {
		log.Info("in-place policy updates: changes edit the active central policy and push it")
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
	// listPrefixes holds the sorted prefixes of the dataprefix lists by id,
	// for the lists vManage lists with their entries.
	listPrefixes map[string][]string
	log          logr.Logger
	// taskWaitTimeout bounds waitTasksFinish; taskPollInterval is its poll period.
	taskWaitTimeout  time.Duration
	taskPollInterval time.Duration
	// inPlace edits the objects of the active central policy and pushes the
	// edit, instead of deactivating the policy for the change.
	inPlace bool
	// locks holds the edit sessions (processId) that in-place PUTs opened,
	// to be released after the push.
	locks []string
}

// refreshCache reloads the IDs of the vManage objects the operator uses, so
// that changes made in vManage since the last event, like a list deleted or
// recreated by hand, are seen. The cache is replaced only if every list was
// read: a response without a data array (e.g. the login page of an expired
// session) is errInvalidResponse.
func (m *sdwanManager) refreshCache() error {
	cache := make(map[string]map[string]string)
	prefixes := make(map[string][]string)
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
			if entries := val.Get("entries"); l.kind == "dataprefix" && entries.IsArray() {
				var p []string
				for _, e := range entries.Array() {
					p = append(p, e.Get("ipPrefix").String())
				}
				slices.Sort(p)
				prefixes[val.Get(l.idField).String()] = p
			}
		}
	}
	m.objectCache = cache
	m.listPrefixes = prefixes

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
	// listData is the dataprefix list to upsert, "" to delete the list, and
	// prefixes are its sorted prefixes.
	listData            string
	prefixes            []string
	port, proto, tunnel string
	// listID and legacyID are the ids of the object's list and of its list
	// from before names were hashed, "" if it has none.
	listID, legacyID string
	// created tells if the change creates the object's list, and
	// listUnchanged if the existing list already has the prefixes.
	created, listUnchanged bool
}

func (u *update) delete() bool { return u.listData == "" }

// updatesList tells if the change updates a list that exists, and may be in
// use.
func (u *update) updatesList() bool { return !u.delete() && !u.created && !u.listUnchanged }

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
	for _, ip := range ipv4 {
		u.prefixes = append(u.prefixes, ip+"/32")
	}
	slices.Sort(u.prefixes)
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
		u.created = !u.delete() && u.listID == ""
		if old, ok := m.listPrefixes[u.listID]; ok && !u.delete() && !u.created {
			u.listUnchanged = slices.Equal(old, u.prefixes)
		}
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
	// is deactivated, to keep the time without a policy short.
	changed, err := m.upsertLists(changed, true, errs)
	if err != nil {
		return fail(err, changed)
	}
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

	// A batch that changes neither a list in use nor the approute
	// definition needs no cycle: e.g. a policy applied again unchanged, or the
	// retry of a delete that failed after its sequences were removed.
	if modified || slices.ContainsFunc(changed, (*update).updatesList) {
		err := m.applyChange(fmt.Sprintf("%d change(s)", len(changed)), func() error {
			// A change whose list vManage rejects keeps its old list and
			// sequences: new sequences over the old list would pair a new
			// port, protocol or tunnel with the old endpoints.
			kept, err := m.upsertLists(changed, false, errs)
			if err != nil {
				return err
			}
			if len(kept) < len(changed) {
				changed = kept
				if newDef, modified, err = replaceSequences(def, changed); err != nil {
					return err
				}
			}
			if !modified {
				return nil
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

// upsertLists creates the lists of the upserts that have none, if created is
// set, or else updates the lists of the others. It returns the updates whose
// list vManage did not reject: a rejected list fails only its change, and any
// other error is returned.
func (m *sdwanManager) upsertLists(updates []*update, created bool, errs []error) ([]*update, error) {
	var kept []*update
	for _, u := range updates {
		if (created && u.created) || (!created && u.updatesList()) {
			if err := m.upsertDataPrefixList(u.objName, u.listData); err != nil {
				if !rejected(err) {
					return updates, err
				}
				errs[u.idx] = err
				continue
			}
			u.listID = m.objectCache["dataprefix"][u.objName]
		}
		kept = append(kept, u)
	}
	return kept, nil
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

// applyChange makes a change to the objects of the central policy. With
// in-place updates the policy stays active: the objects are edited and the
// edit is pushed to the vSmarts, so the edges never run without the policy.
// Otherwise the change is made while the policy is deactivated. Either way
// the policy is activated (pushed) even if the change fails, so that a failed
// change can neither leave the network-wide policy off nor leave edits in
// vManage that the vSmarts do not have.
// An in-place edit opens an edit session in vManage that locks the policy:
// it is released after the push, and if vManage still finds the policy
// locked, the change is made with a cycle, which ends the session.
func (m *sdwanManager) applyChange(op string, change func() error) error {
	if m.inPlace {
		err := change()
		if errors.Is(err, errPolicyLocked) {
			// An edit session vManage still holds: a deactivate ends it. The
			// PUTs before the refused one are made again, unchanged.
			m.log.Error(err, "central policy locked in edit mode, applying the change with a deactivate/activate cycle", "op", op)
			m.releaseLocks()
			return m.cycle(op, change)
		}
		perr := m.pushCentralPolicy()
		m.releaseLocks()
		if perr != nil {
			return errors.Join(err, perr)
		}
		return err
	}
	return m.cycle(op, change)
}

// cycle makes a change while the central policy is deactivated, and activates
// it again even if the change fails.
func (m *sdwanManager) cycle(op string, change func() error) error {
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
			return lockedErr(res, err)
		}
		m.log.Info(fmt.Sprintf("PUT %s: %s, result: %s", putEndpoint, data, res))
		m.recordLock(res)
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
// those of the upserts added. It also tells if the definition changed: if the
// sequences are already as wanted, it returns def.
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
	var removed, added []string
	for _, dpid := range stale {
		for _, seq := range gjson.Get(data, "sequences.#(match.entries.0.ref==\""+dpid+"\")#").Array() {
			removed = append(removed, seq.Raw)
		}
		s := gjson.Get(data, "sequences.#(match.entries.0.ref!=\""+dpid+"\")#")
		var err error
		if data, err = sjson.SetRaw(data, "sequences", s.String()); err != nil {
			return "", false, err
		}
	}

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
			added = append(added, rule)
			sid++
		}
	}

	if sameSequences(removed, added) {
		return def, false, nil
	}
	return data, true, nil
}

// sameSequences tells if two sets of approute sequences differ at most in
// their ids and order. A sequence that does not parse differs.
func sameSequences(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	normalize := func(raws []string) []string {
		out := make([]string, 0, len(raws))
		for _, raw := range raws {
			var r seqRule
			if err := json.Unmarshal([]byte(raw), &r); err != nil {
				return nil
			}
			r.SequenceID = 0
			b, err := json.Marshal(r)
			if err != nil {
				return nil
			}
			out = append(out, string(b))
		}
		slices.Sort(out)
		return out
	}
	na, nb := normalize(a), normalize(b)
	return na != nil && nb != nil && slices.Equal(na, nb)
}

func (m *sdwanManager) putApproute(endpoint, data string) error {
	res, err := m.client.Put(endpoint, data)
	if err != nil {
		m.log.Error(err, fmt.Sprintf("PUT %s: %s, result: %s", endpoint, data, res))
		return lockedErr(res, err)
	}
	m.log.Info(fmt.Sprintf("PUT %s: %s, result: %s", endpoint, data, res))
	m.recordLock(res)
	return nil
}

// lockedErr returns errPolicyLocked, wrapping err, if vManage's response says
// the policy is locked in edit mode, and err otherwise.
func lockedErr(res gjson.Result, err error) error {
	if res.Get("error.code").String() == policyLockedCode {
		return fmt.Errorf("%w: %s: %w", errPolicyLocked, res.Get("error.details").String(), err)
	}
	return err
}

// recordLock keeps the edit session an in-place PUT opened: vManage answers
// a PUT of an object an active policy uses with the processId of the session
// (and the templates it affects), and holds the policy locked until it ends.
func (m *sdwanManager) recordLock(res gjson.Result) {
	if id := res.Get("processId").String(); m.inPlace && id != "" && !slices.Contains(m.locks, id) {
		m.locks = append(m.locks, id)
	}
}

// releaseLocks ends the edit sessions of the in-place PUTs. A failure is
// logged only: the next edit then finds the policy locked, and applies its
// change with a cycle, which ends the session.
func (m *sdwanManager) releaseLocks() {
	for _, id := range m.locks {
		endpoint := "/template/lock/" + id
		res, err := m.client.Delete(endpoint)
		if err != nil {
			m.log.Error(err, fmt.Sprintf("DELETE %s, result: %s", endpoint, res))
			continue
		}
		m.log.Info(fmt.Sprintf("DELETE %s: %s", endpoint, res))
	}
	m.locks = nil
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

// pushCentralPolicy pushes the edited objects of the active central policy
// to the vSmarts: vManage's activate call for an edited policy.
func (m *sdwanManager) pushCentralPolicy() error {
	id, ok := m.objectCache["central"][CentralizedPolicyName]
	if !ok {
		return errors.New("fail to get centralized policy name")
	}
	if err := m.postPolicyTask("/template/policy/vsmart/activate/"+id+"?confirm=true", `{"isEdited":true}`); err != nil {
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

	var data string
	if activate {
		data = "{}"
	}
	return m.postPolicyTask(baseUrl+id, data)
}

// postPolicyTask makes a policy call that starts a vManage task.
func (m *sdwanManager) postPolicyTask(endpoint, data string) error {
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
