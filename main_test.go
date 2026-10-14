package main

import (
	"context"
	"errors"
	"os"
	"testing"

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

func (c *fakeClient) Get(_ context.Context, _ client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
	if c.err != nil {
		return c.err
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
		{"object gone", object.Upserted, nil, apierrors.NewNotFound(schema.GroupResource{Resource: "tunnelpolicyviews"}, "app-TCP-80"), nil, false, false},
		{"get fails", object.Added, nil, transient, nil, true, false},
		{"delete ok", object.Deleted, nil, nil, nil, false, false},
		{"delete fails", object.Deleted, nil, nil, transient, true, false},
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
