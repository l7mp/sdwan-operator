package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
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

	opv1a1 "github.com/l7mp/dcontroller/pkg/api/operator/v1alpha1"
	dmanager "github.com/l7mp/dcontroller/pkg/manager"
	dobject "github.com/l7mp/dcontroller/pkg/object"
	doperator "github.com/l7mp/dcontroller/pkg/operator"
	dreconciler "github.com/l7mp/dcontroller/pkg/reconciler"

	"github.com/l7mp/sdwan-operator/internal/sdwan"
)

const (
	SDWANOperatorSpec               = "artifacts/endpoints-controller-spec.yaml"
	SDWANOperatorGatherSpec         = "artifacts/endpoints-controller-gather-spec.yaml"
	SDWANPolicyTunnelAnnotationName = "policy.sdwan.cisco.com/tunnel"
	SDWANConfigFile                 = "vmanage-config.yaml"
)

var (
	scheme                         = runtime.NewScheme()
	disableEndpointPooling, dryRun *bool
	configFile                     *string
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
}

func main() {
	disableEndpointPooling = flag.Bool("disable-endpoint-pooling", false,
		"Generate per-endpoint objects instead of a single object listing all service endpoints.")
	dryRun = flag.Bool("dry-run", false, "Suppress SD-WAN policy updates.")

	configFile = flag.String("config-file", SDWANConfigFile, "Config file path")

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

	slogHandler := logr.ToSlogHandler(log)
	slog.SetDefault(slog.New(slogHandler))

	if *dryRun {
		log.Info("dry-run mode enabled")
	}

	// Define the controller pipeline
	specFile := SDWANOperatorGatherSpec
	if *disableEndpointPooling {
		specFile = SDWANOperatorSpec
	}

	// Create a dmanager
	mgr, err := dmanager.New(ctrl.GetConfigOrDie(), "sdwan-operator", dmanager.Options{
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
		log.Error(err, "unable to create SDWAN operator")
		os.Exit(1)
	}

	// Read vManage config
	vManageConf := &sdwan.Config{DryRun: *dryRun}
	if !*dryRun {
		c, err := sdwan.ReadConfig(*configFile)
		if err != nil {
			log.Error(err, "unable to read vManage config")
			os.Exit(1)
		}
		vManageConf = c
	}

	// Create the SD-WAN policy controller
	if _, err := NewPolicyController(mgr, logger, *vManageConf); err != nil {
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

// policyController implements the policy controller
type policyController struct {
	client.Client
	log          logr.Logger
	sdwanManager sdwan.Manager
}

func NewPolicyController(mgr manager.Manager, log logr.Logger, sdwanConf sdwan.Config) (*policyController, error) {
	m, err := sdwan.NewManager(sdwanConf, log.WithName("sdwan-mngr"))
	if err != nil {
		return nil, err
	}

	r := &policyController{
		Client:       mgr.GetClient(),
		log:          log.WithName("policy-ctrl"),
		sdwanManager: m,
	}

	on := true
	c, err := controller.NewTyped("sdwan-policy-controller", mgr, controller.TypedOptions[dreconciler.Request]{
		SkipNameValidation: &on,
		Reconciler:         r,
	})
	if err != nil {
		return nil, err
	}

	src, err := dreconciler.NewSource(mgr, "sdwan-operator", opv1a1.Source{
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
	case dobject.Added, dobject.Updated, dobject.Upserted:
		obj := dobject.NewViewObject("sdwan-operator", req.GVK.Kind)
		if err := r.Get(ctx, types.NamespacedName{Name: req.Name, Namespace: req.Namespace}, obj); err != nil {
			r.log.Error(err, "failed to get added/updated object", "delta-type", req.EventType)
			return reconcile.Result{}, err
		}

		spec, ok, err := unstructured.NestedMap(obj.Object, "spec")
		if err != nil || !ok {
			return reconcile.Result{},
				fmt.Errorf("failed to look up added/updated object spec: %q", dobject.Dump(obj))
		}

		name := obj.GetName()
		namespace := obj.GetNamespace()

		r.log.Info("Add/update SD-WAN tunnel policy", "name", name, "namespace", namespace,
			"spec", fmt.Sprintf("%#v", spec))

		// Must use endoint-pooling when using a real manager
		if *dryRun || *disableEndpointPooling {
			return reconcile.Result{}, nil
		}

		port := spec["targetPort"].(int64)
		protocol := spec["protocol"].(string)
		tunnel := spec["tunnel"].(string)
		addresses, ok := spec["addresses"].([]interface{})
		if !ok {
			return reconcile.Result{}, errors.New("unable to parse endpoints from the spec")
		}
		var endpoints []string
		for _, val := range addresses {
			endpoints = append(endpoints, fmt.Sprintf("%v", val))
		}

		err = r.sdwanManager.HandleUpsertEvent(namespace, name, endpoints, port, protocol, tunnel)
		if err != nil {
			r.log.Error(err, "failed to upsert SD-WAN resources")
		}

	case dobject.Deleted:
		r.log.Info("Delete SD-WAN tunnel policy", "name", req.Name, "namespace", req.Namespace)

		// Must use endoint-pooling when using a real manager
		if *dryRun || *disableEndpointPooling {
			return reconcile.Result{}, nil
		}

		err := r.sdwanManager.HandleDeleteEvent(req.Namespace, req.Name)
		if err != nil {
			r.log.Error(err, "failed to delete SD-WAN resources")
		}

	default:
		r.log.Info("Unhandled event for SD-WAN tunnel policy", "name", req.Name, "namespace", req.Namespace, "type", req.EventType)
	}

	r.log.Info("Reconciliation done")

	return reconcile.Result{}, nil
}
