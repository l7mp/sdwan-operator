package sdwan

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
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

var protocolNumbers = map[string]int32{
	"TCP": 6,
	"UDP": 17,
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
	err = m.initCache()
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

func (m *sdwanManager) initCache() error {
	// central policy
	p, err := m.client.Get("/template/policy/vsmart")
	if err != nil {
		return err
	}

	p.Get("data").ForEach(func(key, val gjson.Result) bool {
		if val.Get("policyName").String() == CentralizedPolicyName {
			m.objectCache["central"] = map[string]string{
				CentralizedPolicyName: val.Get("policyId").String(),
			}
			return true
		}
		return false
	})

	// dataprefix list
	dp, err := m.client.Get("/template/policy/list/dataprefix")
	if err != nil {
		return err
	}

	m.objectCache["dataprefix"] = make(map[string]string)
	dp.Get("data").ForEach(func(key, val gjson.Result) bool {
		m.objectCache["dataprefix"][val.Get("name").String()] = val.Get("listId").String()
		return true
	})

	//approute list
	ar, err := m.client.Get("/template/policy/definition/approute")
	if err != nil {
		return err
	}

	m.objectCache["approute"] = make(map[string]string)
	ar.Get("data").ForEach(func(key, val gjson.Result) bool {
		m.objectCache["approute"][val.Get("name").String()] = val.Get("definitionId").String()
		return true
	})

	m.log.Info(fmt.Sprintf("object cache: %#v", m.objectCache))

	return nil
}

func (m *sdwanManager) HandleDeleteEvent(namespace, name string) error {
	objName := convertK8sNameToSdWan(namespace, name)

	if err := m.ensureSession(); err != nil {
		return err
	}

	if err := m.deactivateCentralPolicy(); err != nil {
		m.log.Info(fmt.Sprintf("cannot deactivate central policy, continue delete. err: %s", err))
	}

	// det dataprefixlist id
	dpid, ok := findByPrefix(m.objectCache["dataprefix"], objName)
	if !ok {
		return errors.New("fail to get dataprefix id")
	}

	// update approte seq rules
	id, ok := m.objectCache["approute"][ApproutePolicyName]
	if !ok {
		return errors.New("fail to get approute id")
	}

	endpoint := "/template/policy/definition/approute/" + id
	data, err := m.filterSequenceRules(dpid, endpoint)
	if err != nil {
		return err
	}

	res, err := m.client.Put(endpoint, data)
	if err != nil {
		return err
	}
	m.log.Info(fmt.Sprintf("PUT %s: %s", endpoint, res))

	// delete dataprefix
	k, _ := findKeyByPrefix(m.objectCache["dataprefix"], objName)
	delete(m.objectCache["dataprefix"], k)
	endpoint = "/template/policy/list/dataprefix/" + dpid
	res, err = m.client.Delete(endpoint)
	if err != nil {
		return err
	}
	m.log.Info(fmt.Sprintf("DELETE %s: %s", endpoint, res))

	if err := m.activateCentralPolicy(); err != nil {
		return err
	}

	return nil
}

func (m *sdwanManager) HandleUpsertEvent(namespace, name string, endpoints []string, targetPort int64, protocol, tunnel string) error {
	objName := convertK8sNameToSdWan(namespace, name)

	if err := m.ensureSession(); err != nil {
		return err
	}

	if err := m.deactivateCentralPolicy(); err != nil {
		m.log.Info(fmt.Sprintf("cannot deactivate central policy, continue upsert. err: %s", err))
	}

	// dataprefix
	if err := m.upsertDataPrefixList(objName, endpoints); err != nil {
		return err
	}

	// approute
	port := strconv.Itoa(int(targetPort))
	proto := strconv.Itoa(int(protocolNumbers[protocol]))

	if err := m.updateApproute(objName, port, proto, tunnel); err != nil {
		return err
	}

	if err := m.activateCentralPolicy(); err != nil {
		return err
	}

	return nil
}

func (m *sdwanManager) upsertDataPrefixList(objName string, endpoints []string) error {
	data := m.generateDataPrefixList(objName, endpoints)
	endpoint := "/template/policy/list/dataprefix/"
	id, ok := findByPrefix(m.objectCache["dataprefix"], objName)
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
		m.objectCache["dataprefix"][objName] = res.Get("listId").String()
		m.log.Info(fmt.Sprintf("POST %s: %s, result: %s", endpoint, data, res))
	}

	return nil
}

func (m *sdwanManager) updateApproute(objName, port, proto, tunnel string) error {
	dataPrefixList, ok := findByPrefix(m.objectCache["dataprefix"], objName)
	if !ok {
		return errors.New("fail to get dataprefix")
	}

	id, ok := m.objectCache["approute"][ApproutePolicyName]
	if !ok {
		return errors.New("fail to get approute")
	}

	// get approute without existing seq rules matchin on the dataprefix
	endpoint := "/template/policy/definition/approute/" + id
	desc, err := m.filterSequenceRules(dataPrefixList, endpoint)
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
	dataSrc := m.generateSeqRule(strconv.Itoa(sid), "sourceDataPrefixList", dataPrefixList, "source", port, proto, tunnel)
	dataDst := m.generateSeqRule(strconv.Itoa(sid+1), "destinationDataPrefixList", dataPrefixList, "destination", port, proto, tunnel)

	data, err := sjson.SetRaw(desc, "sequences.-1", dataSrc)
	if err != nil {
		return err
	}
	data, err = sjson.SetRaw(data, "sequences.-1", dataDst)
	if err != nil {
		return err
	}

	res, err := m.client.Put(endpoint, data)
	if err != nil {
		m.log.Error(err, fmt.Sprintf("PUT %s: %s, result: %s", endpoint, data, res))
		return err
	}
	m.log.Info(fmt.Sprintf("PUT %s: %s, result: %s", endpoint, data, res))

	return nil
}

func (m *sdwanManager) filterSequenceRules(dataprefixId, endpoint string) (string, error) {
	desc, err := m.client.Get(endpoint)
	if err != nil {
		return "", err
	}
	s := gjson.Get(desc.String(), "sequences.#(match.entries.0.ref!=\""+dataprefixId+"\")#")
	data, err := sjson.SetRaw(desc.String(), "sequences", s.String())
	if err != nil {
		return "", err
	}

	return data, nil
}

func (m *sdwanManager) generateSeqRule(id, prefixListType, dataPrefixList, direction, port, proto, tunnel string) string {
	return "{\"sequenceId\": " + id + ", \"sequenceName\": \"App Route\", \"sequenceType\": \"appRoute\", \"sequenceIpType\": \"ipv4\", \"match\": {\"entries\": [{\"field\": \"" + prefixListType + "\", \"ref\": \"" + dataPrefixList + "\"}, {\"field\": \"" + direction + "Port\", \"value\": \"" + port + "\"}, {\"field\": \"protocol\", \"value\": \"" + proto + "\"}]}, \"actions\": [{\"type\": \"backupSlaPreferredColor\", \"parameter\": \"" + tunnel + "\"}]}]}"
}

func (m *sdwanManager) generateDataPrefixList(name string, endpoints []string) string {
	prefixes := make([]string, 0, len(endpoints)-1)
	for _, val := range endpoints {
		prefixes = append(prefixes, "{ \"ipPrefix\": \""+val+"/32\"}")
	}

	return "{\"name\": \"" + name + "\", \"description\": \"" + defaultDescription + "\", \"type\": \"dataPrefix\", \"entries\": [" + strings.Join(prefixes, ",") + "]}"
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
