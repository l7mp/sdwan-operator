package sdwan

import (
	"encoding/json"
	"errors"
	"fmt"
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

type Manager interface {
	HandleDeleteEvent(namespace, name string) error
	HandleUpsertEvent(namespace, name string, endpoints []string, targetPort int64, protocol, tunnel string) error
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

type nullManager struct{}

func (m *nullManager) HandleDeleteEvent(_, _ string) error { return nil }
func (m *nullManager) HandleUpsertEvent(_, _ string, _ []string, _ int64, _, _ string) error {
	return nil
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

	m.log.Info(fmt.Sprintf("object cache: %#v", m.objectCache))

	return nil
}

func (m *sdwanManager) HandleDeleteEvent(namespace, name string) error {
	objName := convertK8sNameToSdWan(namespace, name)

	if err := m.ensureSession(); err != nil {
		return err
	}

	if err := m.refreshCache(); err != nil {
		return err
	}

	// det dataprefixlist ids
	var dpids []string
	if dpid, ok := m.objectCache["dataprefix"][objName]; ok {
		dpids = append(dpids, dpid)
	}
	if dpid := m.legacyListID(namespace, name); dpid != "" {
		dpids = append(dpids, dpid)
	}
	if len(dpids) == 0 {
		m.log.Info("no dataprefix list to delete", "name", objName)
		return nil
	}

	// update approte seq rules
	id, ok := m.objectCache["approute"][ApproutePolicyName]
	if !ok {
		return errors.New("fail to get approute id")
	}

	return m.applyChange("delete", func() error {
		endpoint := "/template/policy/definition/approute/" + id
		data, err := m.filterSequenceRules(endpoint, dpids...)
		if err != nil {
			return err
		}

		res, err := m.client.Put(endpoint, data)
		if err != nil {
			return err
		}
		m.log.Info(fmt.Sprintf("PUT %s: %s", endpoint, res))

		// delete dataprefix
		for _, dpid := range dpids {
			if err := m.deleteDataPrefixList(dpid); err != nil {
				return err
			}
		}

		return nil
	})
}

func (m *sdwanManager) HandleUpsertEvent(namespace, name string, endpoints []string, targetPort int64, protocol, tunnel string) error {
	objName := convertK8sNameToSdWan(namespace, name)

	ipv4, protoNum, err := checkUpsert(endpoints, protocol, tunnel)
	if err != nil {
		return err
	}
	if len(ipv4) < len(endpoints) {
		m.log.Info("skipping IPv6 endpoints: approute sequences match IPv4 only", "name", objName)
	}
	// Without endpoints there is nothing to route: remove the service's list
	// rather than sending vManage an empty one.
	if len(ipv4) == 0 {
		return m.HandleDeleteEvent(namespace, name)
	}

	listData, err := m.generateDataPrefixList(objName, ipv4)
	if err != nil {
		return err
	}

	if err := m.ensureSession(); err != nil {
		return err
	}

	if err := m.refreshCache(); err != nil {
		return err
	}

	port := strconv.Itoa(int(targetPort))
	proto := strconv.Itoa(int(protoNum))
	legacyID := m.legacyListID(namespace, name)

	return m.applyChange("upsert", func() error {
		// dataprefix
		if err := m.upsertDataPrefixList(objName, listData); err != nil {
			return err
		}

		// approute
		if err := m.updateApproute(objName, port, proto, tunnel, legacyID); err != nil {
			return err
		}

		// the list from before names were hashed, now unused
		if legacyID != "" {
			return m.deleteDataPrefixList(legacyID)
		}
		return nil
	})
}

// legacyListID returns the id of the list a Kubernetes object has in vManage
// from before names were hashed, or "" if it has none.
func (m *sdwanManager) legacyListID(namespace, name string) string {
	legacy, ok := legacyListName(namespace, name)
	if !ok {
		return ""
	}
	return m.objectCache["dataprefix"][legacy]
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

// updateApproute replaces the approute sequences of a dataprefix list, and
// removes those of the legacy list with the id legacyID, unless that is "".
func (m *sdwanManager) updateApproute(objName, port, proto, tunnel, legacyID string) error {
	dataPrefixList, ok := m.objectCache["dataprefix"][objName]
	if !ok {
		return errors.New("fail to get dataprefix")
	}

	id, ok := m.objectCache["approute"][ApproutePolicyName]
	if !ok {
		return errors.New("fail to get approute")
	}

	// get approute without existing seq rules matchin on the dataprefix
	endpoint := "/template/policy/definition/approute/" + id
	stale := []string{dataPrefixList}
	if legacyID != "" {
		stale = append(stale, legacyID)
	}
	desc, err := m.filterSequenceRules(endpoint, stale...)
	if err != nil {
		return err
	}

	// get sequence ID
	s := gjson.Get(desc, "sequences.#.sequenceId")
	sids := make([]int64, 0, len(s.Array()))
	for _, v := range s.Array() {
		sids = append(sids, v.Int())
	}
	sid := 1
	if len(sids) > 0 {
		sid = int(slices.Max(sids) + 1)
	}

	// install new seqrules
	data := desc
	for i, direction := range []string{"source", "destination"} {
		rule, err := generateSeqRule(sid+i, direction, dataPrefixList, port, proto, tunnel)
		if err != nil {
			return err
		}
		if data, err = sjson.SetRaw(data, "sequences.-1", rule); err != nil {
			return err
		}
	}

	res, err := m.client.Put(endpoint, data)
	if err != nil {
		m.log.Error(err, fmt.Sprintf("PUT %s: %s, result: %s", endpoint, data, res))
		return err
	}
	m.log.Info(fmt.Sprintf("PUT %s: %s, result: %s", endpoint, data, res))

	return nil
}

// filterSequenceRules returns the approute definition at endpoint without the
// sequences that match on any of the dataprefix lists.
func (m *sdwanManager) filterSequenceRules(endpoint string, dataprefixIds ...string) (string, error) {
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
	for _, dpid := range dataprefixIds {
		s := gjson.Get(data, "sequences.#(match.entries.0.ref!=\""+dpid+"\")#")
		if data, err = sjson.SetRaw(data, "sequences", s.String()); err != nil {
			return "", err
		}
	}

	return data, nil
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
