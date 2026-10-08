package releasecontract

import (
	"reflect"
	"strings"
	"testing"

	"github.com/nenya/config"
)

// configSurfaceRoots are the contract type roots for the config/ public API
// surface declared in CONTRACT.md §10a: every exported field of these types
// (and recursively of their exported struct-typed fields) must carry a
// non-empty json tag, so the serialized shape cannot erode silently.
// configInternalTypes are runtime-resolved types declared internal-grade
// (CONTRACT.md §10a guarantee 5): they carry no JSON surface (fields without
// json tags by design) and are excluded from the walk.
var configInternalTypes = map[string]bool{
	"EngineRef":    true, // bouncer engine reference: resolved at startup
	"EngineTarget": true, // resolved engine target: runtime state
}

var configSurfaceRoots = []reflect.Type{

	reflect.TypeOf(config.Config{}),
	reflect.TypeOf(config.SecretsConfig{}),
	reflect.TypeOf(config.ApiKey{}),
	reflect.TypeOf(config.NetworkConfig{}),
}

// TestConfigPublicSurfaceJSONTags enforces CONTRACT.md §10a: every exported
// field on the contract roots (and their exported struct children) carries
// a non-empty json tag with a stable name.
func TestConfigPublicSurfaceJSONTags(t *testing.T) {
	visited := map[reflect.Type]bool{}
	var check func(t *testing.T, typ reflect.Type, path string)
	check = func(t *testing.T, typ reflect.Type, path string) {
		// Resolve indirection first: contract roots hold section types via
		// pointers (Config.Network *NetworkConfig) and maps/slices
		// (Providers map[string]ProviderConfig, Agents map[string]AgentConfig).
		for typ.Kind() == reflect.Ptr || typ.Kind() == reflect.Map || typ.Kind() == reflect.Slice || typ.Kind() == reflect.Array {
			typ = typ.Elem()
		}
		if typ.Kind() != reflect.Struct || visited[typ] || configInternalTypes[typ.Name()] {
			return
		}
		visited[typ] = true
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			if !field.IsExported() {
				continue
			}
			tag := field.Tag.Get("json")
			name := strings.Split(tag, ",")[0]
			if name == "" {
				t.Errorf("%s%s.%s: exported field on the contract surface has no json tag (CONTRACT.md §10a)", path, typ.Name(), field.Name)
			}
			// A json:"-" tag is a declared opt-out (internal-grade runtime
			// state, e.g. BouncerConfig.EngineRef.ResolvedTargets) — the
			// tag itself records the deliberateness, so it is allowed.
			check(t, field.Type, path+typ.Name()+".")
		}
	}
	for _, root := range configSurfaceRoots {
		check(t, root, "")
	}
}

// TestConfigPublicSurfaceMethods enforces the guaranteed resolver API named
// in CONTRACT.md §10a: the methods must exist with the expected names so a
// rename cannot silently break Go consumers.
func TestConfigPublicSurfaceMethods(t *testing.T) {
	gov := reflect.PointerTo(reflect.TypeOf(config.GovernanceConfig{}))
	for _, method := range []string{"EffectiveMaxRetryAttempts", "EffectiveTokenCalibrationParams", "TracingEnabled", "AutoRetryOnContextLimitEnabled"} {
		if _, ok := gov.MethodByName(method); !ok {
			t.Errorf("GovernanceConfig.%s is contract API (CONTRACT.md §10a) but is missing", method)
		}
	}
	prov := reflect.PointerTo(reflect.TypeOf(config.Provider{}))
	for _, method := range []string{"EffectiveCABundle", "EffectiveProxyURL", "EffectiveIdleConnTimeout", "EffectiveResponseHeaderTimeout"} {
		if _, ok := prov.MethodByName(method); !ok {
			t.Errorf("Provider.%s is contract API (CONTRACT.md §10a) but is missing", method)
		}
	}
	net := reflect.PointerTo(reflect.TypeOf(config.NetworkConfig{}))
	if _, ok := net.MethodByName("GetCABundle"); !ok {
		t.Error("NetworkConfig.GetCABundle is contract API (CONTRACT.md §10a) but is missing")
	}
}
