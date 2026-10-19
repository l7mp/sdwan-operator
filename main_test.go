package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/l7mp/dcontroller/pkg/object"
	dreconciler "github.com/l7mp/dcontroller/pkg/reconciler"

	"github.com/l7mp/sdwan-operator/internal/sdwan"
)

func TestMain(m *testing.M) {
	off := false
	dryRun, disableEndpointPooling, inPlaceUpdate = &off, &off, &off
	os.Exit(m.Run())
}

// fakeManager records the batches it applies. It fails the changes of the
// objects named in errs, and the others with err. Apply signals started, and
// waits for block, if they are set.
type fakeManager struct {
	mu      sync.Mutex
	batches [][]sdwan.Change
	errs    map[string]error
	err     error
	started chan struct{}
	block   chan struct{}
}

func (m *fakeManager) Apply(changes []sdwan.Change) []error {
	if m.started != nil {
		m.started <- struct{}{}
	}
	if m.block != nil {
		<-m.block
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.batches = append(m.batches, slices.Clone(changes))
	errs := make([]error, len(changes))
	for i, c := range changes {
		if err, ok := m.errs[c.Name]; ok {
			errs[i] = err
		} else {
			errs[i] = m.err
		}
	}
	return errs
}

func (m *fakeManager) applied() [][]sdwan.Change {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.batches)
}

func (m *fakeManager) setErr(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.err = err
}

// fakeClient serves tunnel policies by name, or fails with err.
type fakeClient struct {
	client.Client
	mu   sync.Mutex
	objs map[string]*unstructured.Unstructured
	err  error
}

func newFakeClient(objs ...*unstructured.Unstructured) *fakeClient {
	c := &fakeClient{objs: map[string]*unstructured.Unstructured{}}
	for _, obj := range objs {
		c.set(obj)
	}
	return c
}

func (c *fakeClient) set(obj *unstructured.Unstructured) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.objs[obj.GetName()] = obj
}

func (c *fakeClient) Get(_ context.Context, key client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return c.err
	}
	found, ok := c.objs[key.Name]
	if !ok {
		return apierrors.NewNotFound(schema.GroupResource{Resource: "tunnelpolicyviews"}, key.Name)
	}
	u := obj.(*unstructured.Unstructured)
	gvk := u.GroupVersionKind()
	u.Object = found.DeepCopy().Object
	u.SetGroupVersionKind(gvk)
	return nil
}

func tunnelPolicy(name string, spec map[string]any) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]any{"spec": spec}}
	obj.SetName(name)
	obj.SetNamespace("default")
	return obj
}

func validSpec() map[string]any {
	return map[string]any{
		"targetPort": int64(8080),
		"protocol":   "TCP",
		"tunnel":     "biz-internet",
		"addresses":  []any{"10.0.0.1"},
	}
}

func request(name string, event object.DeltaType) dreconciler.Request {
	return dreconciler.Request{Namespace: "default", Name: name, EventType: event,
		GVK: schema.GroupVersionKind{Kind: tunnelPolicyKind}}
}

func key(name string) types.NamespacedName {
	return types.NamespacedName{Namespace: "default", Name: name}
}

// newTestController returns a controller that retries after a millisecond and
// does not wait for more changes before a batch.
func newTestController(t *testing.T, c *fakeClient, m *fakeManager) *policyController {
	t.Helper()
	r := &policyController{
		Client:       c,
		log:          logr.Discard(),
		sdwanManager: m,
		queue:        newChangeQueue(time.Millisecond, time.Millisecond),
	}
	t.Cleanup(r.queue.ShutDown)
	return r
}

// Every event was applied on its own, with a cycle of the central policy each,
// and the events of one object were applied one by one, in turn with the
// events of every other object.
func TestReconcileQueuesObjectsOnce(t *testing.T) {
	t.Parallel()
	r := newTestController(t, newFakeClient(), &fakeManager{})
	for _, event := range []object.DeltaType{object.Added, object.Updated, object.Upserted, object.Deleted} {
		if _, err := r.Reconcile(context.Background(), request("app", event)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.Reconcile(context.Background(), request("other", object.Added)); err != nil {
		t.Fatal(err)
	}
	if n := r.queue.Len(); n != 2 {
		t.Fatalf("%d objects queued, want 2", n)
	}
}

// A batch applies the current state of each queued object: an upsert if it
// exists, a delete if it is gone.
func TestApplyBatchAppliesCurrentState(t *testing.T) {
	t.Parallel()
	// without endpoint pooling the pipeline emits a single address
	single := validSpec()
	delete(single, "addresses")
	single["address"] = "10.0.0.9"
	m := &fakeManager{}
	r := newTestController(t, newFakeClient(tunnelPolicy("app-1", validSpec()), tunnelPolicy("app-2", single)), m)
	for _, name := range []string{"app-1", "app-2", "gone"} {
		r.queue.Add(key(name))
	}

	if !r.applyBatch(context.Background()) {
		t.Fatal("applyBatch stopped")
	}
	want := []sdwan.Change{
		{Namespace: "default", Name: "app-1", Endpoints: []string{"10.0.0.1"}, TargetPort: 8080, Protocol: "TCP", Tunnel: "biz-internet"},
		{Namespace: "default", Name: "app-2", Endpoints: []string{"10.0.0.9"}, TargetPort: 8080, Protocol: "TCP", Tunnel: "biz-internet"},
		{Namespace: "default", Name: "gone", Delete: true},
	}
	if got := m.applied(); len(got) != 1 || !reflect.DeepEqual(got[0], want) {
		t.Fatalf("applied %#v, want one batch %#v", got, want)
	}
	if n := r.queue.Len(); n != 0 {
		t.Fatalf("%d objects left in the queue", n)
	}
}

// A delete retried after the object was created again removed the new
// object's list and sequences. The current state of the object is applied
// instead, whatever the event was.
func TestRecreatedObjectIsUpserted(t *testing.T) {
	t.Parallel()
	m := &fakeManager{}
	r := newTestController(t, newFakeClient(tunnelPolicy("app", validSpec())), m)
	if _, err := r.Reconcile(context.Background(), request("app", object.Deleted)); err != nil {
		t.Fatal(err)
	}

	r.applyBatch(context.Background())
	if got := m.applied(); len(got) != 1 || len(got[0]) != 1 || got[0][0].Delete {
		t.Fatalf("applied %#v, want an upsert", got)
	}
}

func TestApplyBatchIsCapped(t *testing.T) {
	t.Parallel()
	m := &fakeManager{}
	r := newTestController(t, newFakeClient(), m)
	for i := range maxBatch + 3 {
		r.queue.Add(key(fmt.Sprintf("app-%d", i)))
	}

	r.applyBatch(context.Background())
	r.applyBatch(context.Background())
	if got := m.applied(); len(got) != 2 || len(got[0]) != maxBatch || len(got[1]) != 3 {
		t.Fatalf("applied batches of %d changes, want %d and 3", len(got[0]), maxBatch)
	}
}

// Errors from vManage were logged and Reconcile returned nil, so a failed
// change was never retried.
func TestApplyBatchRetries(t *testing.T) {
	t.Parallel()
	transient := errors.New("vManage unreachable")
	for _, tc := range []struct {
		name    string
		spec    map[string]any // nil: the object is gone
		getErr  error
		mgrErr  error
		applied bool
		retry   bool
	}{
		{"upsert ok", validSpec(), nil, nil, true, false},
		{"upsert fails", validSpec(), nil, transient, true, true},
		{"upsert with invalid tunnel", validSpec(), nil, sdwan.ErrInvalidSpec, true, false},
		{"spec without protocol", map[string]any{"targetPort": int64(8080), "tunnel": "mpls", "addresses": []any{}}, nil, nil, false, false},
		{"named port", map[string]any{"targetPort": "http", "protocol": "TCP", "tunnel": "mpls", "addresses": []any{}}, nil, nil, false, false},
		{"get fails", validSpec(), transient, nil, false, true},
		{"delete ok", nil, nil, nil, true, false},
		{"delete fails", nil, nil, transient, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := newFakeClient()
			if tc.spec != nil {
				c.set(tunnelPolicy("app", tc.spec))
			}
			c.err = tc.getErr
			m := &fakeManager{err: tc.mgrErr}
			r := newTestController(t, c, m)
			r.queue.Add(key("app"))

			r.applyBatch(context.Background())
			if applied := len(m.applied()) == 1; applied != tc.applied {
				t.Fatalf("applied: %v, want %v", applied, tc.applied)
			}
			if retry := r.queue.NumRequeues(key("app")) == 1; retry != tc.retry {
				t.Fatalf("retried: %v, want %v", retry, tc.retry)
			}
		})
	}
}

// A change that keeps failing was retried forever, and every attempt
// deactivated and activated the central policy.
func TestApplyBatchGivesUpEventually(t *testing.T) {
	t.Parallel()
	m := &fakeManager{err: errors.New("vManage refuses")}
	r := newTestController(t, newFakeClient(tunnelPolicy("app", validSpec())), m)
	r.queue.Add(key("app"))

	for range maxAttempts {
		r.applyBatch(context.Background())
	}
	time.Sleep(20 * time.Millisecond) // a retry would be queued by now
	if n, attempts := r.queue.Len(), len(m.applied()); n != 0 || attempts != maxAttempts {
		t.Fatalf("%d attempts and %d queued after giving up, want %d and 0", attempts, n, maxAttempts)
	}

	// the count starts over after a success
	r.queue.Add(key("app"))
	r.applyBatch(context.Background())
	m.setErr(nil)
	r.applyBatch(context.Background())
	if n := r.queue.NumRequeues(key("app")); n != 0 {
		t.Fatalf("%d retries counted after a success", n)
	}
}

// An object that changes while its batch is applied is applied again after,
// once, with its newest state.
func TestChangeDuringBatchIsAppliedOnceAfter(t *testing.T) {
	t.Parallel()
	m := &fakeManager{started: make(chan struct{}, 4), block: make(chan struct{})}
	c := newFakeClient(tunnelPolicy("app", validSpec()))
	r := newTestController(t, c, m)
	r.queue.Add(key("app"))

	done := make(chan struct{})
	go func() {
		r.applyBatch(context.Background())
		close(done)
	}()
	<-m.started
	newer := validSpec()
	newer["addresses"] = []any{"10.0.0.2"}
	c.set(tunnelPolicy("app", newer))
	for range 2 {
		if _, err := r.Reconcile(context.Background(), request("app", object.Updated)); err != nil {
			t.Fatal(err)
		}
	}
	close(m.block)
	<-done

	r.applyBatch(context.Background())
	got := m.applied()
	if len(got) != 2 || len(got[1]) != 1 || !reflect.DeepEqual(got[1][0].Endpoints, []string{"10.0.0.2"}) {
		t.Fatalf("applied %#v, want the newest state once after the first batch", got)
	}
	if n := r.queue.Len(); n != 0 {
		t.Fatalf("%d objects left in the queue", n)
	}
}

// A batch collected when the operator stops is not applied: the shutdown
// would have to wait for its cycle.
func TestRunBatchesStopsOnShutdown(t *testing.T) {
	t.Parallel()
	m := &fakeManager{}
	r := newTestController(t, newFakeClient(), m)
	r.batchDelay = time.Minute
	r.queue.Add(key("app"))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.runBatches(ctx) }()
	for r.queue.Len() > 0 { // the batch is being collected
		time.Sleep(time.Millisecond)
	}
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runBatches does not return after shutdown")
	}
	if got := m.applied(); len(got) != 0 {
		t.Fatalf("applied %#v after shutdown", got)
	}
}

// targetPort given as a string or a float was rejected: NestedInt64 failed on
// anything but an int64 before the other types were tried.
func TestGetTargetPort(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		raw  any
		want int64
		ok   bool
	}{
		{"int64", int64(8080), 8080, true},
		{"int", 8080, 8080, true},
		{"int32", int32(8080), 8080, true},
		{"float64", 8080.0, 8080, true},
		{"string", "8080", 8080, true},
		{"fractional float64", 8080.5, 0, false},
		{"named port", "http", 0, false},
		{"zero", int64(0), 0, false},
		{"too large", int64(65536), 0, false},
		{"too large float64", 1e20, 0, false},
		{"bool", true, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := getTargetPort(map[string]any{"targetPort": tc.raw})
			if tc.ok && (err != nil || got != tc.want) {
				t.Fatalf("getTargetPort(%#v) = %d, %v; want %d", tc.raw, got, err, tc.want)
			}
			if !tc.ok && err == nil {
				t.Fatalf("getTargetPort(%#v) = %d, want an error", tc.raw, got)
			}
		})
	}
	if _, err := getTargetPort(map[string]any{}); err == nil {
		t.Fatal("missing targetPort accepted")
	}
}

func blockUntilDone(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

func runAsync(ctx context.Context, r *policyController, timeout time.Duration) <-chan error {
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, logr.Discard(), blockUntilDone, blockUntilDone, make(chan error), r.waitChanges, timeout)
	}()
	return done
}

// On shutdown the error reporter called os.Exit(1), which would also end this
// test binary.
func TestRunReturnsOnShutdown(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	done := runAsync(ctx, &policyController{}, time.Second)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run after shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run does not return after shutdown")
	}
}

// A change cut short by shutdown can leave the central policy deactivated, so
// shutdown waits for the change in progress, and no new change starts.
func TestRunWaitsForChangeInProgress(t *testing.T) {
	t.Parallel()
	m := &fakeManager{}
	r := newTestController(t, newFakeClient(), m)
	r.changing.RLock() // a change in progress
	ctx, cancel := context.WithCancel(context.Background())
	done := runAsync(ctx, r, time.Minute)
	cancel()

	select {
	case err := <-done:
		t.Fatalf("run returned during a change: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	r.changing.RUnlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run after the change: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run does not return after the change finished")
	}

	r.queue.Add(key("app"))
	go r.applyBatch(context.Background())
	time.Sleep(100 * time.Millisecond)
	if got := m.applied(); len(got) != 0 {
		t.Fatalf("a change started after shutdown: %#v", got)
	}
}

func TestRunGivesUpOnStuckChange(t *testing.T) {
	t.Parallel()
	r := &policyController{}
	r.changing.RLock() // never finishes
	ctx, cancel := context.WithCancel(context.Background())
	done := runAsync(ctx, r, 50*time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "still in progress") {
			t.Fatalf("expected a timeout error, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run does not give up on a stuck change")
	}
}

// A failed API server was only logged: the operator kept running without it.
func TestRunStopsWhenAPIServerFails(t *testing.T) {
	t.Parallel()
	r := &policyController{}
	failServer := func(context.Context) error { return errors.New("address in use") }
	done := make(chan error, 1)
	go func() {
		done <- run(context.Background(), logr.Discard(), blockUntilDone, failServer, make(chan error), r.waitChanges, time.Second)
	}()

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "API server: address in use") {
			t.Fatalf("expected the API server error, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("operator keeps running after the API server failed")
	}
}

// An API server that stops because of the shutdown is no error.
func TestRunIgnoresAPIServerStopOnShutdown(t *testing.T) {
	t.Parallel()
	r := &policyController{}
	stopServer := func(ctx context.Context) error {
		<-ctx.Done()
		return errors.New("server closed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, logr.Discard(), blockUntilDone, stopServer, make(chan error), r.waitChanges, time.Second)
	}()
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run after shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run does not return after shutdown")
	}
}

// With -disable-endpoint-pooling, Reconcile returned before it did anything,
// so the flag silently disabled the operator.
func TestCheckFlags(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ dryRun, noPooling, ok bool }{
		{false, false, true},
		{true, false, true},
		{true, true, true},
		{false, true, false},
	} {
		if err := checkFlags(tc.dryRun, tc.noPooling); (err == nil) != tc.ok {
			t.Fatalf("checkFlags(dryRun=%v, disableEndpointPooling=%v) = %v", tc.dryRun, tc.noPooling, err)
		}
	}
}

// The -in-place-update flag enables in-place updates on top of the config
// file, so that the mode can be switched without editing the credentials.
func TestReadVManageConfigInPlaceFlag(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, tc := range []struct {
		file       string
		flag, want bool
	}{
		{"url: https://vmanage\n", false, false},
		{"url: https://vmanage\n", true, true},
		{"url: https://vmanage\nin-place-update: true\n", false, true},
	} {
		path := dir + "/vmanage-config.yaml"
		if err := os.WriteFile(path, []byte(tc.file), 0o600); err != nil {
			t.Fatal(err)
		}
		c, err := readVManageConfig(false, tc.flag, path)
		if err != nil || c.InPlaceUpdate != tc.want {
			t.Fatalf("file %q, flag %v: InPlaceUpdate %v (err %v), want %v", tc.file, tc.flag, c != nil && c.InPlaceUpdate, err, tc.want)
		}
	}
	if c, err := readVManageConfig(true, true, dir+"/missing.yaml"); err != nil || !c.DryRun {
		t.Fatalf("dry run: %+v, %v", c, err)
	}
}

// A shutdown during a long change returned the manager's grace period error,
// so the operator exited 1 even though the change finished.
func TestRunShutdownIgnoresStopError(t *testing.T) {
	t.Parallel()
	r := &policyController{}
	stopLate := func(ctx context.Context) error {
		<-ctx.Done()
		return errors.New("failed waiting for all runnables to end within grace period")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, logr.Discard(), stopLate, blockUntilDone, make(chan error), r.waitChanges, time.Second)
	}()
	cancel()

	if err := <-done; err != nil {
		t.Fatalf("run after a clean shutdown: %v", err)
	}
	failStart := func(context.Context) error { return errors.New("cannot start") }
	if err := run(context.Background(), logr.Discard(), failStart, blockUntilDone, make(chan error), (&policyController{}).waitChanges, time.Second); err == nil {
		t.Fatal("start failure not reported")
	}
}
