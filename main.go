package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"math"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"go.uber.org/zap/zapcore"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	opv1a1 "github.com/l7mp/dcontroller/pkg/api/operator/v1alpha1"
	"github.com/l7mp/dcontroller/pkg/apiserver"
	"github.com/l7mp/dcontroller/pkg/cache"
	"github.com/l7mp/dcontroller/pkg/object"
	doperator "github.com/l7mp/dcontroller/pkg/operator"
	dreconciler "github.com/l7mp/dcontroller/pkg/reconciler"

	"github.com/l7mp/sdwan-operator/internal/sdwan"
)

const (
	SDWANOperatorSpec               = "artifacts/endpoints-controller-spec.yaml"
	SDWANOperatorGatherSpec         = "artifacts/endpoints-controller-gather-spec.yaml"
	SDWANPolicyTunnelAnnotationName = "policy.sdwan.cisco.com/tunnel"
	SDWANConfigFile                 = "vmanage-config.yaml"
	apiServerPort                   = 8443
	maxPort                         = 65535
	retryBaseDelay                  = time.Second
	retryMaxDelay                   = 5 * time.Minute
	// changeTimeout bounds the wait for a change at shutdown: a change waits
	// for the vManage tasks twice, at most 5 minutes each.
	changeTimeout = 11 * time.Minute
)

var (
	scheme                         = runtime.NewScheme()
	disableEndpointPooling, dryRun *bool
	configFile                     *string
	server                         *apiserver.APIServer
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
}

func NewOperator(specFile string, logger logr.Logger) (*doperator.Operator, *policyController, error) {
	log := logger.WithName("sdwan-op")
	ctrl.SetLogger(log)

	// Create a dmanager
	config := ctrl.GetConfigOrDie()
	api, err := cache.NewAPI(config, cache.APIOptions{
		CacheOptions: cache.CacheOptions{Logger: logger},
	})
	if err != nil {
		return nil, nil, err
	}

	apiServerConfig, err := apiserver.NewDefaultConfig("", apiServerPort, api.Client, true, false, logger)
	if err != nil {
		return nil, nil, err
	}
	server, err = apiserver.NewAPIServer(apiServerConfig)
	if err != nil {
		return nil, nil, err
	}

	// Load the operator from file
	errorChan := make(chan error, 16)
	opts := doperator.Options{
		Cache:        api.GetCache(),
		ErrorChannel: errorChan,
		APIServer:    server,
		Logger:       logger,
	}

	op, err := doperator.NewFromFile("sdwan-operator", config, specFile, opts)
	if err != nil {
		return nil, nil, fmt.Errorf("unable to create SDWAN operator: %w", err)
	}

	// Read vManage config
	vManageConf := &sdwan.Config{DryRun: *dryRun}
	if !*dryRun {
		c, err := sdwan.ReadConfig(*configFile)
		if err != nil {
			return nil, nil, fmt.Errorf("unable to read vManage config: %w", err)
		}
		vManageConf = c
	}

	// Create the SD-WAN policy controller
	pc, err := NewPolicyController(op.GetManager(), logger, *vManageConf)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create policy controller: %w", err)
	}

	log.Info("created SDWAN policy controller")

	return op, pc, nil
}

// policyController implements the policy controller
type policyController struct {
	client.Client
	log          logr.Logger
	sdwanManager sdwan.Manager
	// changing is read-locked while an SD-WAN change is in progress.
	changing sync.RWMutex
}

// waitChanges waits until no SD-WAN change is in progress, for at most
// timeout, and keeps new changes from starting. It returns false on timeout.
func (r *policyController) waitChanges(timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		r.changing.Lock()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
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
		// Every attempt deactivates and activates the central policy, so
		// retry far less eagerly than the 5ms default.
		RateLimiter: workqueue.NewTypedItemExponentialFailureRateLimiter[dreconciler.Request](retryBaseDelay, retryMaxDelay),
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

	// A change interrupted between deactivating and activating the central
	// policy would leave it off: shutdown waits for it.
	r.changing.RLock()
	defer r.changing.RUnlock()

	if err := r.reconcileEvent(ctx, req); err != nil {
		// A policy that cannot be applied as specified is not retried; any
		// other error is, with backoff.
		if errors.Is(err, sdwan.ErrInvalidSpec) {
			err = reconcile.TerminalError(err)
		}
		return reconcile.Result{}, err
	}

	r.log.Info("Reconciliation done")

	return reconcile.Result{}, nil
}

func (r *policyController) reconcileEvent(ctx context.Context, req dreconciler.Request) error {
	// vManage update
	switch req.EventType {
	case object.Added, object.Updated, object.Upserted:
		obj := object.NewViewObject("sdwan-operator", req.GVK.Kind)
		if err := r.Get(ctx, types.NamespacedName{Name: req.Name, Namespace: req.Namespace}, obj); err != nil {
			if apierrors.IsNotFound(err) {
				// deleted since: its delete event removes the SD-WAN resources
				r.log.Info("SD-WAN tunnel policy gone, skipping", "name", req.Name, "namespace", req.Namespace)
				return nil
			}
			return fmt.Errorf("failed to get added/updated object: %w", err)
		}

		spec, ok, err := unstructured.NestedMap(obj.Object, "spec")
		if err != nil || !ok {
			return fmt.Errorf("%w: failed to look up added/updated object spec: %q", sdwan.ErrInvalidSpec, object.Dump(obj))
		}

		name := obj.GetName()
		namespace := obj.GetNamespace()

		r.log.Info("Add/update SD-WAN tunnel policy", "name", name, "namespace", namespace,
			"spec", fmt.Sprintf("%#v", spec))

		port, protocol, tunnel, endpoints, err := parseSpec(spec)
		if err != nil {
			return fmt.Errorf("%w: %w", sdwan.ErrInvalidSpec, err)
		}

		if err := r.sdwanManager.HandleUpsertEvent(namespace, name, endpoints, port, protocol, tunnel); err != nil {
			return fmt.Errorf("failed to upsert SD-WAN resources: %w", err)
		}

	case object.Deleted:
		r.log.Info("Delete SD-WAN tunnel policy", "name", req.Name, "namespace", req.Namespace)

		// A delete retried after the object was created again must not remove
		// the new object's SD-WAN resources. The view cache drops an object
		// before it reports the delete, so a found object is a new one.
		obj := object.NewViewObject("sdwan-operator", req.GVK.Kind)
		err := r.Get(ctx, types.NamespacedName{Name: req.Name, Namespace: req.Namespace}, obj)
		if err == nil {
			r.log.Info("SD-WAN tunnel policy created again, skipping delete", "name", req.Name, "namespace", req.Namespace)
			return nil
		}
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to check deleted object: %w", err)
		}

		if err := r.sdwanManager.HandleDeleteEvent(req.Namespace, req.Name); err != nil {
			return fmt.Errorf("failed to delete SD-WAN resources: %w", err)
		}

	default:
		r.log.Info("Unhandled event for SD-WAN tunnel policy", "name", req.Name, "namespace", req.Namespace, "type", req.EventType)
	}

	return nil
}

// parseSpec returns the fields of a tunnel policy spec.
func parseSpec(spec map[string]interface{}) (port int64, protocol, tunnel string, endpoints []string, err error) {
	port, err = getTargetPort(spec)
	if err != nil {
		return 0, "", "", nil, fmt.Errorf("unable to parse targetPort from spec: %w", err)
	}

	protocol, ok, err := unstructured.NestedString(spec, "protocol")
	if err != nil || !ok {
		return 0, "", "", nil, fmt.Errorf("unable to parse protocol from spec: %v", err)
	}

	tunnel, ok, err = unstructured.NestedString(spec, "tunnel")
	if err != nil || !ok {
		return 0, "", "", nil, fmt.Errorf("unable to parse tunnel from spec: %v", err)
	}

	// without endpoint pooling, a spec has a single address
	if address, ok, err := unstructured.NestedString(spec, "address"); err == nil && ok {
		return port, protocol, tunnel, []string{address}, nil
	}

	addresses, ok, err := unstructured.NestedSlice(spec, "addresses")
	if err != nil || !ok {
		return 0, "", "", nil, fmt.Errorf("unable to parse addresses from spec: %v", err)
	}

	endpoints = make([]string, 0, len(addresses))
	for i, val := range addresses {
		address, ok := val.(string)
		if !ok {
			return 0, "", "", nil, fmt.Errorf("unable to parse address %d from spec: expected string, got %T", i, val)
		}
		endpoints = append(endpoints, address)
	}

	return port, protocol, tunnel, endpoints, nil
}

// getTargetPort returns the target port of a spec. A Service gives it as a
// number or a string, and a number may be decoded as an integer or a float.
// Named ports are not supported.
func getTargetPort(spec map[string]interface{}) (int64, error) {
	raw, ok := spec["targetPort"]
	if !ok {
		return 0, fmt.Errorf("field missing")
	}

	var port int64
	switch v := raw.(type) {
	case int64:
		port = v
	case int:
		port = int64(v)
	case int32:
		port = int64(v)
	case float64:
		if v != math.Trunc(v) || v < 1 || v > maxPort {
			return 0, fmt.Errorf("invalid port %v", v)
		}
		port = int64(v)
	case string:
		p, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("named port %q is not supported, use a port number", v)
		}
		port = p
	default:
		return 0, fmt.Errorf("unexpected type %T", raw)
	}
	if port < 1 || port > maxPort {
		return 0, fmt.Errorf("invalid port %d", port)
	}

	return port, nil
}

func main() {
	disableEndpointPooling = flag.Bool("disable-endpoint-pooling", false,
		"Generate per-endpoint objects instead of a single object listing all service endpoints (requires -dry-run).")
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
	log := logger.WithName("setup")
	slogHandler := logr.ToSlogHandler(log)
	slog.SetDefault(slog.New(slogHandler))

	if err := checkFlags(*dryRun, *disableEndpointPooling); err != nil {
		log.Error(err, "invalid flags")
		os.Exit(1)
	}
	if *dryRun {
		log.Info("dry-run mode enabled")
	}

	// Define the controller pipeline
	specFile := SDWANOperatorGatherSpec
	if *disableEndpointPooling {
		specFile = SDWANOperatorSpec
	}

	ctx := ctrl.SetupSignalHandler()

	op, pc, err := NewOperator(specFile, logger)
	if err != nil {
		log.Error(err, "problem running operator")
		os.Exit(1)
	}

	if err := run(ctx, log, op.Start, server.Start, op.GetErrorChannel(), pc.waitChanges, changeTimeout); err != nil {
		log.Error(err, "problem running operator")
		os.Exit(1)
	}
}

// checkFlags rejects flag combinations the operator cannot run with.
func checkFlags(dryRun, disableEndpointPooling bool) error {
	// The SD-WAN manager keeps one list of all endpoints per service.
	if disableEndpointPooling && !dryRun {
		return errors.New("-disable-endpoint-pooling requires -dry-run: SD-WAN policies need endpoint pooling")
	}
	return nil
}

// run runs the operator and its API server until ctx is done or the API
// server fails. It then waits for the SD-WAN change in progress, if any, for at
// most timeout.
func run(ctx context.Context, log logr.Logger, startOp, startServer func(context.Context) error,
	errs <-chan error, waitChanges func(time.Duration) bool, timeout time.Duration) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	go func() {
		err := startServer(ctx)
		if ctx.Err() != nil {
			return // shutting down
		}
		if err == nil {
			err = errors.New("stopped")
		}
		cancel(fmt.Errorf("API server: %w", err))
	}()

	// Create an error reporter thread
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case err := <-errs:
				log.Error(err, "operator error")
			}
		}
	}()

	err := startOp(ctx)
	if cause := context.Cause(ctx); !errors.Is(cause, context.Canceled) {
		err = errors.Join(err, cause)
	}
	if !waitChanges(timeout) {
		err = errors.Join(err, fmt.Errorf("SD-WAN change still in progress after %s", timeout))
	}

	return err
}
