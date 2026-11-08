package istio

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/metadata/metadatainformer"
	toolscache "k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// Read-only RBAC (D-036/D-037): the chart renders these in a separate
// istio-reader ClusterRole unless integrations.istio.enabled=false; CRD
// metadata watch lives in the main role.
// +kubebuilder:rbac:groups=networking.istio.io,resources=virtualservices;gateways,verbs=get;list;watch
// +kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,verbs=get;list;watch

// Mode is the integration policy from values (integrations.istio.enabled).
type Mode string

const (
	// ModeAuto: run when Istio is detected (default).
	ModeAuto Mode = "auto"
	// ModeEnabled: required; a warning is logged while Istio is missing.
	ModeEnabled Mode = "true"
	// ModeDisabled: never run.
	ModeDisabled Mode = "false"
)

// ParseMode accepts auto|true|false (case-insensitive; empty = auto).
func ParseMode(s string) (Mode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "auto":
		return ModeAuto, nil
	case "true":
		return ModeEnabled, nil
	case "false":
		return ModeDisabled, nil
	}
	return "", fmt.Errorf("invalid istio integration mode %q (want auto|true|false)", s)
}

// Status values (D-036 vocabulary).
const (
	StatusDisabled    = "disabled"
	StatusNotDetected = "notDetected"
	StatusMissing     = "missing"
	StatusActive      = "active"
	StatusDegraded    = "degraded"
)

// Group is the Istio networking API group.
const Group = "networking.istio.io"

// candidateVersions are tried in order (preferred first).
var candidateVersions = []string{"v1", "v1beta1"}

// crdNames are the CRDs whose appearance/removal triggers re-detection.
var crdNames = []string{"virtualservices." + Group, "gateways." + Group}

var crdGVR = schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}

// DefaultRecheckInterval is the sparse discovery fallback (D-036: ≤10m).
const DefaultRecheckInterval = 10 * time.Minute

// syncWarnAfter: log a warning when the Istio informers have not synced
// after this long (they keep trying; the snapshot appears once synced).
const syncWarnAfter = 30 * time.Second

// Integration is a manager.Runnable owning the Istio informers.
type Integration struct {
	Mode      Mode
	Discovery discovery.DiscoveryInterface
	Dynamic   dynamic.Interface
	// Metadata (optional) watches the Istio CRDs so install/uninstall is
	// picked up without polling; nil → discovery recheck only.
	Metadata metadata.Interface
	// RecheckInterval is the discovery fallback period (default 10m).
	RecheckInterval time.Duration

	snap   atomic.Pointer[Snapshot]
	status atomic.Value // string

	mu        sync.Mutex
	listeners map[int]func()
	nextL     int

	// run state — touched only by the Start goroutine.
	running       *runState
	warnedMissing bool
	warnedDenied  bool
	kick          chan struct{}
}

type runState struct {
	gv schema.GroupVersion
	// hasGW records whether the gateways informer runs: gateways being
	// served (or not) independently of virtualservices requires a restart.
	hasGW   bool
	factory dynamicinformer.DynamicSharedInformerFactory
	stop    chan struct{}
	done    chan struct{}
}

// New returns an Integration with defaults.
func New(mode Mode, disc discovery.DiscoveryInterface, dyn dynamic.Interface, md metadata.Interface) *Integration {
	i := &Integration{Mode: mode, Discovery: disc, Dynamic: dyn, Metadata: md}
	i.status.Store(StatusNotDetected)
	return i
}

// Snapshot returns the current parsed state (nil when inactive). Nil-safe.
func (i *Integration) Snapshot() *Snapshot {
	if i == nil {
		return nil
	}
	return i.snap.Load()
}

// Status returns the current integration status. Nil-safe.
func (i *Integration) Status() string {
	if i == nil {
		return StatusDisabled
	}
	if s, ok := i.status.Load().(string); ok {
		return s
	}
	return StatusNotDetected
}

// Subscribe registers fn to be called (synchronously, from the integration
// goroutine — keep it non-blocking) whenever the snapshot changes. Nil-safe.
func (i *Integration) Subscribe(fn func()) (cancel func()) {
	if i == nil || fn == nil {
		return func() {}
	}
	i.mu.Lock()
	if i.listeners == nil {
		i.listeners = map[int]func(){}
	}
	id := i.nextL
	i.nextL++
	i.listeners[id] = fn
	i.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			i.mu.Lock()
			delete(i.listeners, id)
			i.mu.Unlock()
		})
	}
}

// publish swaps the snapshot and notifies listeners only on a real change.
func (i *Integration) publish(s *Snapshot) {
	old := i.snap.Load()
	if (old == nil) == (s == nil) && reflect.DeepEqual(old, s) {
		return
	}
	i.snap.Store(s)
	i.mu.Lock()
	fns := make([]func(), 0, len(i.listeners))
	for _, fn := range i.listeners {
		fns = append(fns, fn)
	}
	i.mu.Unlock()
	for _, fn := range fns {
		fn()
	}
}

// NeedLeaderElection — read-only, every replica keeps its own snapshot.
func (i *Integration) NeedLeaderElection() bool { return false }

func (i *Integration) recheckInterval() time.Duration {
	if i.RecheckInterval <= 0 || i.RecheckInterval > DefaultRecheckInterval {
		return DefaultRecheckInterval
	}
	return i.RecheckInterval
}

// Start implements manager.Runnable. It never fails the manager: every
// problem (not installed, forbidden, discovery errors) degrades to a log
// line and an empty snapshot; the topology mapper keeps working.
func (i *Integration) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("istio")
	if i.Mode == ModeDisabled {
		i.status.Store(StatusDisabled)
		logger.Info("istio integration disabled (integrations.istio.enabled=false)")
		return nil
	}
	i.kick = make(chan struct{}, 1)
	i.watchCRDs(ctx)

	ticker := time.NewTicker(i.recheckInterval())
	defer ticker.Stop()
	i.reconcile(ctx)
	for {
		select {
		case <-ctx.Done():
			i.stopInformers()
			return nil
		case <-i.kick:
			i.reconcile(ctx)
		case <-ticker.C:
			i.reconcile(ctx)
		}
	}
}

func (i *Integration) requestReconcile() {
	select {
	case i.kick <- struct{}{}:
	default:
	}
}

// watchCRDs starts metadata-only informers on the two Istio CRDs (filtered
// by name) so install/uninstall triggers re-detection immediately. When the
// CRD watch is not permitted it falls back to the sparse recheck.
func (i *Integration) watchCRDs(ctx context.Context) {
	logger := log.FromContext(ctx).WithName("istio")
	if i.Metadata == nil {
		return
	}
	h := toolscache.ResourceEventHandlerFuncs{
		AddFunc:    func(interface{}) { i.requestReconcile() },
		UpdateFunc: func(interface{}, interface{}) { i.requestReconcile() },
		DeleteFunc: func(interface{}) { i.requestReconcile() },
	}
	for _, name := range crdNames {
		sel := "metadata.name=" + name
		probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_, err := i.Metadata.Resource(crdGVR).List(probeCtx, metav1.ListOptions{FieldSelector: sel, Limit: 1})
		cancel()
		if err != nil {
			logger.Info("WARNING: cannot watch Istio CRDs; falling back to periodic discovery recheck",
				"interval", i.recheckInterval(), "error", err.Error())
			return
		}
		inf := metadatainformer.NewFilteredMetadataInformer(i.Metadata, crdGVR, "", 0, nil,
			func(o *metav1.ListOptions) { o.FieldSelector = sel }).Informer()
		if _, err := inf.AddEventHandler(h); err != nil {
			logger.Info("WARNING: cannot attach CRD watch handler", "error", err.Error())
			return
		}
		go inf.Run(ctx.Done())
	}
}

// detect finds the served networking.istio.io version that has
// virtualservices (preferred v1, fallback v1beta1) and whether gateways are
// served in that version. gv is empty when Istio is not installed.
func (i *Integration) detect() (gv schema.GroupVersion, hasGateways bool, err error) {
	for _, v := range candidateVersions {
		rl, err := i.Discovery.ServerResourcesForGroupVersion(Group + "/" + v)
		if err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return schema.GroupVersion{}, false, err
		}
		hasVS := false
		for _, r := range rl.APIResources {
			switch r.Name {
			case "virtualservices":
				hasVS = true
			case "gateways":
				hasGateways = true
			}
		}
		if hasVS {
			return schema.GroupVersion{Group: Group, Version: v}, hasGateways, nil
		}
		hasGateways = false
	}
	return schema.GroupVersion{}, false, nil
}

// reconcile aligns the running informers with policy ∧ detection ∧ RBAC.
func (i *Integration) reconcile(ctx context.Context) {
	logger := log.FromContext(ctx).WithName("istio")
	gv, hasGW, err := i.detect()
	if err != nil {
		logger.Info("WARNING: istio discovery failed; keeping current state", "error", err.Error())
		return
	}
	if gv.Empty() {
		if i.running != nil {
			logger.Info("istio API no longer served; stopping istio informers")
			i.stopInformers()
		}
		if i.Mode == ModeEnabled {
			i.status.Store(StatusMissing)
			if !i.warnedMissing {
				logger.Info("WARNING: integrations.istio.enabled=true but networking.istio.io (virtualservices) is not served by the cluster")
				i.warnedMissing = true
			}
		} else {
			i.status.Store(StatusNotDetected)
		}
		return
	}
	i.warnedMissing = false
	if i.running != nil && i.running.gv == gv && i.running.hasGW == hasGW {
		return
	}
	if i.running != nil {
		logger.Info("istio API surface changed; restarting istio informers",
			"groupVersion", gv.String(), "gateways", hasGW)
		i.stopInformers()
	}
	if err := i.startInformers(ctx, gv, hasGW); err != nil {
		i.status.Store(StatusDegraded)
		if !i.warnedDenied {
			logger.Info("WARNING: istio detected but cannot be read; integration degraded (the map keeps working without it)",
				"groupVersion", gv.String(), "error", err.Error(),
				"fix", "grant get/list/watch on networking.istio.io virtualservices,gateways (chart: istio-reader ClusterRole)")
			i.warnedDenied = true
		}
		return
	}
	i.warnedDenied = false
	i.status.Store(StatusActive)
	logger.Info("istio integration active", "groupVersion", gv.String(), "gateways", hasGW)
}

// startInformers probes access (a Forbidden list must not leave a retrying
// informer behind), then runs a private dynamic informer factory with its
// own stop channel.
func (i *Integration) startInformers(ctx context.Context, gv schema.GroupVersion, hasGW bool) error {
	logger := log.FromContext(ctx).WithName("istio")
	vsGVR := gv.WithResource("virtualservices")
	gwGVR := gv.WithResource("gateways")
	gvrs := []schema.GroupVersionResource{vsGVR}
	if hasGW {
		gvrs = append(gvrs, gwGVR)
	}
	for _, gvr := range gvrs {
		probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_, err := i.Dynamic.Resource(gvr).List(probeCtx, metav1.ListOptions{Limit: 1})
		cancel()
		if err != nil {
			return fmt.Errorf("list %s: %w", gvr.Resource, err)
		}
	}

	factory := dynamicinformer.NewDynamicSharedInformerFactory(i.Dynamic, 0)
	rs := &runState{gv: gv, hasGW: hasGW, factory: factory, stop: make(chan struct{}), done: make(chan struct{})}
	changed := make(chan struct{}, 1)
	h := toolscache.ResourceEventHandlerFuncs{
		AddFunc:    func(interface{}) { signal(changed) },
		UpdateFunc: func(interface{}, interface{}) { signal(changed) },
		DeleteFunc: func(interface{}) { signal(changed) },
	}
	vsInf := factory.ForResource(vsGVR).Informer()
	var gwInf toolscache.SharedIndexInformer
	if hasGW {
		gwInf = factory.ForResource(gwGVR).Informer()
	}
	synced := []toolscache.InformerSynced{}
	for _, inf := range []toolscache.SharedIndexInformer{vsInf, gwInf} {
		if inf == nil {
			continue
		}
		var warned atomic.Bool
		_ = inf.SetWatchErrorHandler(func(_ *toolscache.Reflector, err error) {
			if apierrors.IsForbidden(err) && !warned.Swap(true) {
				logger.Info("WARNING: istio watch forbidden; snapshot may be stale", "error", err.Error())
			}
		})
		if _, err := inf.AddEventHandler(h); err != nil {
			return err
		}
		synced = append(synced, inf.HasSynced)
	}
	factory.Start(rs.stop)

	go func() {
		defer close(rs.done)
		// Publish only after the initial list so consumers never see a
		// partial snapshot.
		syncDone := make(chan bool, 1)
		go func() { syncDone <- toolscache.WaitForCacheSync(rs.stop, synced...) }()
		select {
		case ok := <-syncDone:
			if !ok {
				return
			}
		case <-time.After(syncWarnAfter):
			logger.Info("WARNING: istio informers not synced yet; still waiting", "after", syncWarnAfter)
			if ok := <-syncDone; !ok {
				return
			}
		case <-rs.stop:
			return
		}
		i.publish(buildSnapshot(vsInf, gwInf))
		for {
			select {
			case <-rs.stop:
				return
			case <-changed:
				i.publish(buildSnapshot(vsInf, gwInf))
			}
		}
	}()
	i.running = rs
	return nil
}

// stopInformers stops the private factory and clears the snapshot.
func (i *Integration) stopInformers() {
	rs := i.running
	if rs == nil {
		return
	}
	i.running = nil
	close(rs.stop)
	rs.factory.Shutdown()
	<-rs.done
	i.publish(nil)
}

func signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// buildSnapshot parses the informer stores into a sorted snapshot.
func buildSnapshot(vsInf, gwInf toolscache.SharedIndexInformer) *Snapshot {
	s := &Snapshot{VirtualServices: []VirtualService{}, Gateways: []Gateway{}}
	for _, obj := range vsInf.GetStore().List() {
		if u, ok := obj.(*unstructured.Unstructured); ok {
			s.VirtualServices = append(s.VirtualServices, ParseVirtualService(u))
		}
	}
	if gwInf != nil {
		for _, obj := range gwInf.GetStore().List() {
			if u, ok := obj.(*unstructured.Unstructured); ok {
				s.Gateways = append(s.Gateways, ParseGateway(u))
			}
		}
	}
	sortSnapshot(s)
	return s
}
