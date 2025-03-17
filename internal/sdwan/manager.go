package sdwan

import (
	"errors"
	"fmt"
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
	defaultDescription    = "managed by SD-WAN operator"
)

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
	sc, err := sdwan.NewClient(c.URL, c.User, c.Password, c.Insecure)
	if err != nil {
		return nil, err
	}

	m := &sdwanManager{
		client:      sc,
		objectCache: make(map[string]map[string]string),
		log:         log,
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

	//nolint:errcheck
	m.deactivateCentralPolicy()

	// approute
	id, ok := m.objectCache["approute"][objName]
	if ok {
		delete(m.objectCache["approute"], objName)
		endpoint := "/template/policy/definition/approute/" + id
		res, err := m.client.Delete(endpoint)
		if err != nil {
			return err
		}
		m.log.Info(fmt.Sprintf("DELETE %s: %s", endpoint, res))
	}

	// dataprefix
	id, ok = m.objectCache["dataprefix"][objName]
	if ok {
		delete(m.objectCache["dataprefix"], objName)
		endpoint := "/template/policy/list/dataprefix/" + id
		res, err := m.client.Delete(endpoint)
		if err != nil {
			return err
		}
		m.log.Info(fmt.Sprintf("DELETE %s: %s", endpoint, res))
	}

	if err := m.activateCentralPolicy(); err != nil {
		return err
	}

	return nil
}

func (m *sdwanManager) HandleUpsertEvent(namespace, name string, endpoints []string, targetPort int64, protocol, tunnel string) error {
	objName := convertK8sNameToSdWan(namespace, name)

	//nolint:errcheck
	m.deactivateCentralPolicy()

	// dataprefix
	if err := m.upsertDataPrefixList(objName, endpoints); err != nil {
		return err
	}

	// approute
	port := strconv.Itoa(int(targetPort))
	proto := strconv.Itoa(int(protocolNumbers[protocol]))

	if err := m.upsertApproute(objName, port, proto, tunnel); err != nil {
		return err
	}

	if err := m.activateCentralPolicy(); err != nil {
		return err
	}

	return nil
}

func (m *sdwanManager) upsertDataPrefixList(objName string, endpoints []string) error {
	prefixes := []string{}
	for _, val := range endpoints {
		prefixes = append(prefixes, "{ \"ipPrefix\": \""+val+"/32\"}")
	}

	endpoint := "/template/policy/list/dataprefix/"
	data := "{\"name\": \"" + objName + "\", \"description\": \"" + defaultDescription + "\", \"type\": \"dataPrefix\", \"entries\": [" + strings.Join(prefixes, ",") + "]}"

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
		m.objectCache["dataprefix"][objName] = res.Get("listId").String()
		m.log.Info(fmt.Sprintf("POST %s: %s, result: %s", endpoint, data, res))
	}

	return nil
}

func (m *sdwanManager) upsertApproute(objName, port, proto, tunnel string) error {
	dataPrefixList, ok := m.objectCache["dataprefix"][objName]
	if !ok {
		return errors.New("fail to get dataprefix for new approute")
	}

	endpoint := "/template/policy/definition/approute/"
	data := "{\"name\": \"" + objName + "\", \"description\": \"" + defaultDescription + "\", \"type\": \"appRoute\", \"sequences\": [{\"sequenceId\": 1, \"sequenceName\": \"App Route\", \"sequenceType\": \"appRoute\", \"sequenceIpType\": \"ipv4\", \"match\": {\"entries\": [{\"field\": \"sourceDataPrefixList\", \"ref\": \"" + dataPrefixList + "\"}, {\"field\": \"sourcePort\", \"value\": \"" + port + "\"},{\"field\": \"protocol\", \"value\": \"" + proto + "\"}]}, \"actions\": [{\"type\": \"backupSlaPreferredColor\", \"parameter\": \"" + tunnel + "\"}]}, {\"sequenceId\": 11, \"sequenceName\": \"App Route\", \"sequenceType\": \"appRoute\", \"sequenceIpType\": \"ipv4\", \"match\": {\"entries\": [{\"field\": \"destinationDataPrefixList\", \"ref\": \"" + dataPrefixList + "\"}, {\"field\": \"destinationPort\", \"value\": \"" + port + "\"}, {\"field\": \"protocol\", \"value\": \"" + proto + "\"}]}, \"actions\": [{\"type\": \"backupSlaPreferredColor\", \"parameter\": \"" + tunnel + "\"}]}]}"

	id, ok := m.objectCache["approute"][objName]
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
		id := res.Get("definitionId").String()
		m.objectCache["approute"][objName] = id
		m.log.Info(fmt.Sprintf("POST %s: %s, result: %s", endpoint, data, res))

		if err := m.registerApproute(id); err != nil {
			return err
		}
	}

	return nil
}

func (m *sdwanManager) registerApproute(id string) error {
	cpid, ok := m.objectCache["central"][CentralizedPolicyName]
	if !ok {
		return errors.New("failed to get centralized policy name")
	}
	endpoint := "/template/policy/vsmart/definition/" + cpid

	desc, err := m.client.Get(endpoint)
	if err != nil {
		return err
	}

	template := desc.Get("policyDefinition.assembly.0")
	newVal, err := sjson.Set(template.String(), "definitionId", id)
	if err != nil {
		return err
	}
	data, err := sjson.SetRaw(desc.String(), "policyDefinition.assembly.-1", newVal)
	if err != nil {
		return err
	}

	res, err := m.client.Put(endpoint, data)
	if err != nil {
		m.log.Error(err, fmt.Sprintf(">>> PUT %s: %s, result: %s", endpoint, data, res))
		return err
	}
	m.log.Info(fmt.Sprintf("PUT %s: %s, result: %s", endpoint, data, res))
	return nil
}

func (m *sdwanManager) deactivateCentralPolicy() error {
	ret := m.toggleCentralPolicy(false)
	m.waitTasksFinish()
	return ret
}

func (m *sdwanManager) activateCentralPolicy() error {
	ret := m.toggleCentralPolicy(true)
	m.waitTasksFinish()
	return ret
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
	data := "{}"
	res, err := m.client.Post(endpoint, data)
	if err != nil {
		return err
	}
	m.log.Info(fmt.Sprintf("POST %s: %s, result: %s", endpoint, data, res))

	return nil
}

func (m *sdwanManager) waitTasksFinish() {
	endpoint := "/device/action/status/tasks"
	for {
		ret, err := m.client.Get(endpoint)
		if err != nil {
			m.log.Error(err, fmt.Sprintf("GET %s", endpoint))
		}
		if ret.Get("runningTasks").String() == "[]" {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}
