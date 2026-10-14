package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/l7mp/dcontroller/pkg/object"
	dreconciler "github.com/l7mp/dcontroller/pkg/reconciler"

	"github.com/l7mp/sdwan-operator/internal/sdwan"
)

func TestMain(m *testing.M) {
	off := false
	dryRun, disableEndpointPooling = &off, &off
	os.Exit(m.Run())
}

// fakeManager records the SD-WAN changes and fails them with err.
type fakeManager struct {
	err              error
	upserts, deletes int
	endpoints        []string
}

func (m *fakeManager) HandleDeleteEvent(_, _ string) error {
	m.deletes++
	return m.err
}

func (m *fakeManager) HandleUpsertEvent(_, _ string, endpoints []string, _ int64, _, _ string) error {
	m.upserts++
	m.endpoints = endpoints
	return m.err
}

// fakeClient serves a single object, or fails with err.
type fakeClient struct {
	client.Client
	obj *unstructured.Unstructured
	err error
}

func (c *fakeClient) Get(_ context.Context, key client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
	if c.err != nil {
		return c.err
	}
	if c.obj == nil {
		return apierrors.NewNotFound(schema.GroupResource{Resource: "tunnelpolicyviews"}, key.Name)
	}
	u := obj.(*unstructured.Unstructured)
	gvk := u.GroupVersionKind()
	u.Object = c.obj.DeepCopy().Object
	u.SetGroupVersionKind(gvk)
	return nil
}

func tunnelPolicy(spec map[string]any) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]any{"spec": spec}}
	obj.SetName("app-TCP-80")
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

func request(event object.DeltaType) dreconciler.Request {
	return dreconciler.Request{Namespace: "default", Name: "app-TCP-80", EventType: event,
		GVK: schema.GroupVersionKind{Kind: "TunnelPolicyView"}}
}

// Errors from vManage were logged and Reconcile returned nil, so a failed
// change was never retried.
func TestReconcileRetries(t *testing.T) {
	t.Parallel()
	transient := errors.New("vManage unreachable")
	gone := apierrors.NewNotFound(schema.GroupResource{Resource: "tunnelpolicyviews"}, "app-TCP-80")
	for _, tc := range []struct {
		name     string
		event    object.DeltaType
		spec     map[string]any
		getErr   error
		mgrErr   error
		retry    bool // a non-terminal error
		terminal bool
	}{
		{"upsert ok", object.Added, validSpec(), nil, nil, false, false},
		{"upsert fails", object.Upserted, validSpec(), nil, transient, true, false},
		{"upsert with invalid tunnel", object.Updated, validSpec(), nil, sdwan.ErrInvalidSpec, false, true},
		{"spec without protocol", object.Added, map[string]any{"targetPort": int64(8080), "tunnel": "mpls", "addresses": []any{}}, nil, nil, false, true},
		{"named port", object.Added, map[string]any{"targetPort": "http", "protocol": "TCP", "tunnel": "mpls", "addresses": []any{}}, nil, nil, false, true},
		{"object gone", object.Upserted, nil, gone, nil, false, false},
		{"get fails", object.Added, nil, transient, nil, true, false},
		{"delete ok", object.Deleted, nil, gone, nil, false, false},
		{"delete fails", object.Deleted, nil, gone, transient, true, false},
		{"delete cannot check the object", object.Deleted, nil, transient, nil, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := &fakeManager{err: tc.mgrErr}
			r := &policyController{
				Client:       &fakeClient{obj: tunnelPolicy(tc.spec), err: tc.getErr},
				log:          logr.Discard(),
				sdwanManager: m,
			}

			_, err := r.Reconcile(context.Background(), request(tc.event))
			terminal := errors.Is(err, reconcile.TerminalError(nil))
			switch {
			case tc.retry && (err == nil || terminal):
				t.Fatalf("expected an error to retry, got %v", err)
			case tc.terminal && !terminal:
				t.Fatalf("expected a terminal error, got %v", err)
			case !tc.retry && !tc.terminal && err != nil:
				t.Fatalf("expected no error, got %v", err)
			}
		})
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
	r := &policyController{log: logr.Discard(), sdwanManager: &fakeManager{}}
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

	started := make(chan struct{})
	go func() {
		_, _ = r.Reconcile(context.Background(), request(object.Deleted))
		close(started)
	}()
	select {
	case <-started:
		t.Fatal("a change started after shutdown")
	case <-time.After(100 * time.Millisecond):
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

// Without endpoint pooling the pipeline emits a single address, which the
// reconciler did not read.
func TestReconcileReadsSingleAddress(t *testing.T) {
	t.Parallel()
	spec := validSpec()
	delete(spec, "addresses")
	spec["address"] = "10.0.0.1"
	m := &fakeManager{}
	r := &policyController{Client: &fakeClient{obj: tunnelPolicy(spec)}, log: logr.Discard(), sdwanManager: m}

	if _, err := r.Reconcile(context.Background(), request(object.Added)); err != nil {
		t.Fatal(err)
	}
	if m.upserts != 1 || len(m.endpoints) != 1 || m.endpoints[0] != "10.0.0.1" {
		t.Fatalf("upserted %d times with %v, want once with [10.0.0.1]", m.upserts, m.endpoints)
	}
}

// A failed delete is retried with backoff. If the object was created again by
// then, the retry removed the new object's list and sequences.
func TestRetriedDeleteSparesRecreatedObject(t *testing.T) {
	t.Parallel()
	m := &fakeManager{}
	r := &policyController{Client: &fakeClient{obj: tunnelPolicy(validSpec())}, log: logr.Discard(), sdwanManager: m}

	if _, err := r.Reconcile(context.Background(), request(object.Deleted)); err != nil {
		t.Fatal(err)
	}
	if m.deletes != 0 {
		t.Fatalf("delete of a recreated object removed its SD-WAN resources")
	}
}

// A change that keeps failing was retried forever, and every attempt
// deactivated and activated the central policy.
func TestReconcileGivesUpEventually(t *testing.T) {
	t.Parallel()
	m := &fakeManager{err: errors.New("vManage refuses")}
	r := &policyController{Client: &fakeClient{obj: tunnelPolicy(validSpec())}, log: logr.Discard(), sdwanManager: m}

	for i := 1; i <= maxAttempts; i++ {
		_, err := r.Reconcile(context.Background(), request(object.Added))
		if terminal := errors.Is(err, reconcile.TerminalError(nil)); err == nil || terminal != (i == maxAttempts) {
			t.Fatalf("attempt %d: got %v", i, err)
		}
	}

	// the count starts over, and a success resets it
	m.err = nil
	if _, err := r.Reconcile(context.Background(), request(object.Added)); err != nil {
		t.Fatal(err)
	}
	m.err = errors.New("vManage refuses")
	for i := 1; i < maxAttempts; i++ {
		if _, err := r.Reconcile(context.Background(), request(object.Added)); errors.Is(err, reconcile.TerminalError(nil)) {
			t.Fatalf("attempt %d after a success is terminal: %v", i, err)
		}
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
