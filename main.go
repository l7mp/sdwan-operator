// A simplified vertical pod autoscaler implementation.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/go-logr/logr"
	"go.uber.org/zap/zapcore"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	opv1a1 "github.com/hsnlab/dcontroller/pkg/api/operator/v1alpha1"
	"github.com/hsnlab/dcontroller/pkg/cache"
	dmanager "github.com/hsnlab/dcontroller/pkg/manager"
	dobject "github.com/hsnlab/dcontroller/pkg/object"
	doperator "github.com/hsnlab/dcontroller/pkg/operator"
	dreconciler "github.com/hsnlab/dcontroller/pkg/reconciler"
)

const (
	SDWANOperatorSpec               = "artifacts/endpoints-controller-spec.yaml"
	SDWANOperatorGatherSpec         = "artifacts/endpoints-controller-gather-spec.yaml"
	SDWANPolicyTunnelAnnotationName = "policy.sdwan.cisco.com/tunnel"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
}

func main() {
	gatherEndpoints := flag.Bool("gather-endpoints", false,
		"Generate a single object per service with all endpoints.")

	zapOpts := zap.Options{
		Development:     true,
		DestWriter:      os.Stderr,
		StacktraceLevel: zapcore.Level(3),
		TimeEncoder:     zapcore.RFC3339NanoTimeEncoder,
	}
	zapOpts.BindFlags(flag.CommandLine)
	flag.Parse()

	logger := zap.New(zap.UseFlagOptions(&zapOpts))
	log := logger.WithName("sdwan-op")
	ctrl.SetLogger(log)

	specFile := SDWANOperatorSpec
	if *gatherEndpoints {
		specFile = SDWANOperatorGatherSpec
	}

	// Create a dmanager
	mgr, err := dmanager.New(ctrl.GetConfigOrDie(), dmanager.Options{
		Options: ctrl.Options{Scheme: scheme},
	})
	if err != nil {
		log.Error(err, "unable to set up dmanager")
		os.Exit(1)
	}

	// Load the operator from file
	errorChan := make(chan error, 16)
	opts := doperator.Options{
		ErrorChannel: errorChan,
		Logger:       logger,
	}

	if _, err := doperator.NewFromFile("sdwan-operator", mgr, specFile, opts); err != nil {
		log.Error(err, "unable to create pod autoscaler operator")
		os.Exit(1)
	}

	// Create the SD-WAN policy controller
	if _, err := NewPolicyController(mgr, logger); err != nil {
		log.Error(err, "failed to create policy controller")
		os.Exit(1)
	}

	log.Info("created SDWAN policy controller")

	// Create an error reporter thread
	ctx := ctrl.SetupSignalHandler()
	go func() {
		for {
			select {
			case <-ctx.Done():
				os.Exit(1)
			case err := <-errorChan:
				log.Error(err, "operator error")
			}
		}
	}()

	if err := mgr.Start(ctx); err != nil {
		log.Error(err, "problem running operator")
		os.Exit(1)
	}
}

// implement the policy controller
type policyController struct {
	client.Client
	log logr.Logger
}

func NewPolicyController(mgr manager.Manager, log logr.Logger) (*policyController, error) {
	r := &policyController{
		Client: mgr.GetClient(),
		log:    log.WithName("policy-ctrl"),
	}

	on := true
	c, err := controller.NewTyped("sdwan-policy-controller", mgr, controller.TypedOptions[dreconciler.Request]{
		SkipNameValidation: &on,
		Reconciler:         r,
	})
	if err != nil {
		return nil, err
	}

	src, err := dreconciler.NewSource(mgr, opv1a1.Source{
		Resource: opv1a1.Resource{
			Kind: "TunnelPolicyView",
		},
	}).GetSource()
	if err != nil {
		return nil, fmt.Errorf("failed to create source: %w", err)
	}

	if err := c.Watch(src); err != nil {
		return nil, fmt.Errorf("failed to create watch: %w", err)
	}
	r.log.Info("created SDWAN policy controller")

	return r, nil
}

func (r *policyController) Reconcile(ctx context.Context, req dreconciler.Request) (reconcile.Result, error) {
	r.log.Info("Reconciling", "request", req.String())

	// vManage update
	switch req.EventType {
	case cache.Added, cache.Updated, cache.Upserted:
		obj := dobject.NewViewObject(req.GVK.Kind)
		if err := r.Get(ctx, types.NamespacedName{Name: req.Name, Namespace: req.Namespace}, obj); err != nil {
			r.log.Error(err, "failed to get added/updated object", "delta-type", req.EventType)
			return reconcile.Result{}, err
		}

		spec, ok, err := unstructured.NestedMap(obj.Object, "spec")
		if err != nil || !ok {
			return reconcile.Result{},
				fmt.Errorf("failed to look up added/updated object spec: %q", dobject.Dump(obj))
		}

		r.log.Info("Add/update SD-WAN tunnel policy", "name", obj.GetName(), "namespace", obj.GetNamespace(),
			"spec", fmt.Sprintf("%#v", spec))

	case cache.Deleted:
		r.log.Info("Delete SD-WAN tunnel policy", "name", req.Name, "namespace", req.Namespace)

	default:
		r.log.Info("Unhandled event for SD-WAN tunnel policy", "name", req.Name, "namespace", req.Namespace,
			"type", req.EventType)
	}

	return reconcile.Result{}, nil
}
