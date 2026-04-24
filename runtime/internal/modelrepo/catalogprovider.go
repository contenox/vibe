package modelrepo

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/contenox/contenox/libtracker"
)

// BackendSpec is the runtime-independent input needed to talk to a model catalog.
// It deliberately excludes DB/KV concerns; callers resolve those before construction.
type BackendSpec struct {
	Type    string
	BaseURL string
	APIKey  string
}

// ObservedModel is the normalized result of listing models from a backend.
// Name is the provider-facing model identifier used for selection and execution.
type ObservedModel struct {
	Name          string
	ContextLength int
	ModifiedAt    time.Time
	Size          int64
	Digest        string
	CapabilityConfig
	Meta map[string]string
}

// CatalogProvider observes the models exposed by one backend instance and can
// turn an observed model into the existing execution Provider abstraction.
type CatalogProvider interface {
	Type() string
	ListModels(ctx context.Context) ([]ObservedModel, error)
	ProviderFor(model ObservedModel) Provider
}

// CatalogFactory constructs CatalogProvider implementations from backend specs.
type CatalogFactory interface {
	NewCatalogProvider(spec BackendSpec, opts ...CatalogOption) (CatalogProvider, error)
}

// CatalogOptions carries optional construction dependencies used by vendor implementations.
type CatalogOptions struct {
	HTTPClient *http.Client
	Tracker    libtracker.ActivityTracker
}

// CatalogOption mutates CatalogOptions before a provider is constructed.
type CatalogOption func(*CatalogOptions)

// WithCatalogHTTPClient overrides the HTTP client used for observation and Provider construction.
func WithCatalogHTTPClient(client *http.Client) CatalogOption {
	return func(opts *CatalogOptions) {
		opts.HTTPClient = client
	}
}

// WithCatalogTracker injects the tracker used by ProviderFor when building execution Providers.
func WithCatalogTracker(tracker libtracker.ActivityTracker) CatalogOption {
	return func(opts *CatalogOptions) {
		opts.Tracker = tracker
	}
}

// CatalogProviderConstructor is the registry tools implemented by vendor packages.
type CatalogProviderConstructor func(spec BackendSpec, opts CatalogOptions) (CatalogProvider, error)

type registryCatalogFactory struct{}

var (
	catalogRegistryMu sync.RWMutex
	catalogRegistry   = map[string]CatalogProviderConstructor{}
)

// RegisterCatalogProvider registers a backend catalog implementation by type.
// Vendor packages call this from init() to avoid import cycles from modelrepo -> vendor packages.
func RegisterCatalogProvider(backendType string, constructor CatalogProviderConstructor) {
	normalized := strings.ToLower(strings.TrimSpace(backendType))
	if normalized == "" {
		panic("modelrepo: catalog provider type cannot be empty")
	}
	if constructor == nil {
		panic("modelrepo: catalog provider constructor cannot be nil")
	}

	catalogRegistryMu.Lock()
	defer catalogRegistryMu.Unlock()
	if _, exists := catalogRegistry[normalized]; exists {
		panic("modelrepo: catalog provider already registered for type " + normalized)
	}
	catalogRegistry[normalized] = constructor
}

// DefaultCatalogFactory returns the registry-backed factory used by runtimestate.
func DefaultCatalogFactory() CatalogFactory {
	return registryCatalogFactory{}
}

// NewCatalogProvider constructs a registry-backed catalog provider.
func NewCatalogProvider(spec BackendSpec, opts ...CatalogOption) (CatalogProvider, error) {
	return DefaultCatalogFactory().NewCatalogProvider(spec, opts...)
}

func (registryCatalogFactory) NewCatalogProvider(spec BackendSpec, opts ...CatalogOption) (CatalogProvider, error) {
	normalized := strings.ToLower(strings.TrimSpace(spec.Type))
	if normalized == "" {
		return nil, fmt.Errorf("catalog provider type is required")
	}

	options := CatalogOptions{
		HTTPClient: http.DefaultClient,
		Tracker:    libtracker.NoopTracker{},
	}
	for _, opt := range opts {
		if opt != nil {
			opt(&options)
		}
	}
	if options.HTTPClient == nil {
		options.HTTPClient = http.DefaultClient
	}
	if options.Tracker == nil {
		options.Tracker = libtracker.NoopTracker{}
	}

	catalogRegistryMu.RLock()
	constructor, ok := catalogRegistry[normalized]
	catalogRegistryMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unsupported catalog provider type %q", spec.Type)
	}

	spec.Type = normalized
	return constructor(spec, options)
}
