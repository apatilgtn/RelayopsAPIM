// Package apiops implements the RelayOps APIOps delivery engine:
// bundle compilation, release passports, environment orchestration,
// and gate-protected deployment lifecycles.
package apiops

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// EnvironmentOverlay defines environment-specific configuration overrides.
// Overrides may replace upstream endpoints, secret bindings, and rate limits,
// but MUST NOT weaken authentication policies or drop required tests.
type EnvironmentOverlay struct {
	UpstreamURLs map[string]string `json:"upstream_urls,omitempty" yaml:"upstream_urls,omitempty"` // api_name -> url
	SecretRefs   map[string]string `json:"secret_refs,omitempty" yaml:"secret_refs,omitempty"`     // name -> ref
	Limits       map[string]int    `json:"limits,omitempty" yaml:"limits,omitempty"`               // api_name -> rate_limit
}

// CompiledBundle is the canonical delivery artifact generated from repository sources.
type CompiledBundle struct {
	FormatVersion string            `json:"format_version"`
	Environment   string            `json:"environment,omitempty"`
	SourceHash    string            `json:"source_hash"`
	RenderedHash  string            `json:"rendered_hash"`
	Config        DeclarativeConfig `json:"config"`
	Files         []string          `json:"files"`
}

// CompileDirectory scans an APIOps repository directory structure, compiles
// all modular API and plan definitions, applies any requested environment overlay,
// and returns the canonical CompiledBundle with cryptographic source and rendered digests.
func CompileDirectory(root string, env string) (*CompiledBundle, error) {
	fi, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("read repository directory: %w", err)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("path %q is not a directory", root)
	}

	var scannedFiles []string
	sourceHasher := sha256.New()

	hashFile := func(path string, data []byte) {
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		scannedFiles = append(scannedFiles, rel)
		sourceHasher.Write([]byte(rel + "\x00"))
		sourceHasher.Write(data)
		sourceHasher.Write([]byte("\x00"))
	}

	apisDir := filepath.Join(root, "apis")
	plansDir := filepath.Join(root, "plans")
	envsDir := filepath.Join(root, "environments")

	var apis []DeclAPI
	var plans []DeclPlan

	// 1. Process APIs
	if fi, err := os.Stat(apisDir); err == nil && fi.IsDir() {
		entries, err := os.ReadDir(apisDir)
		if err != nil {
			return nil, fmt.Errorf("read apis dir: %w", err)
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			apiName := entry.Name()
			dirPath := filepath.Join(apisDir, apiName)

			var apiCfg DeclAPI
			var foundConfig bool

			for _, cfgName := range []string{"config.yaml", "config.yml", "config.json"} {
				cfgPath := filepath.Join(dirPath, cfgName)
				if b, err := os.ReadFile(cfgPath); err == nil {
					hashFile(cfgPath, b)
					jsonBytes := b
					if !strings.HasSuffix(cfgName, ".json") {
						j, err := yamlToJSON(b)
						if err != nil {
							return nil, fmt.Errorf("%s: invalid yaml: %w", cfgPath, err)
						}
						jsonBytes = j
					}
					if err := json.Unmarshal(jsonBytes, &apiCfg); err != nil {
						return nil, fmt.Errorf("%s: invalid config: %w", cfgPath, err)
					}
					foundConfig = true
					break
				}
			}
			if !foundConfig {
				continue
			}
			if apiCfg.Name == "" {
				apiCfg.Name = apiName
			}

			// Check for optional openapi spec
			for _, specName := range []string{"openapi.yaml", "openapi.yml", "openapi.json"} {
				specPath := filepath.Join(dirPath, specName)
				if b, err := os.ReadFile(specPath); err == nil {
					hashFile(specPath, b)
					var specObj map[string]any
					if strings.HasSuffix(specName, ".json") {
						_ = json.Unmarshal(b, &specObj)
					} else {
						_ = yaml.Unmarshal(b, &specObj)
					}
					if len(specObj) > 0 {
						apiCfg.OpenAPISpec = specObj
					}
					break
				}
			}

			apis = append(apis, apiCfg)
		}
	}

	// 2. Process Plans
	if fi, err := os.Stat(plansDir); err == nil && fi.IsDir() {
		entries, err := os.ReadDir(plansDir)
		if err != nil {
			return nil, fmt.Errorf("read plans dir: %w", err)
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			name := entry.Name()
			if !strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml") && !strings.HasSuffix(name, ".json") {
				continue
			}
			planPath := filepath.Join(plansDir, name)
			b, err := os.ReadFile(planPath)
			if err != nil {
				continue
			}
			hashFile(planPath, b)

			var p DeclPlan
			jsonBytes := b
			if !strings.HasSuffix(name, ".json") {
				j, err := yamlToJSON(b)
				if err != nil {
					return nil, fmt.Errorf("%s: invalid yaml: %w", planPath, err)
				}
				jsonBytes = j
			}
			if err := json.Unmarshal(jsonBytes, &p); err != nil {
				return nil, fmt.Errorf("%s: invalid plan: %w", planPath, err)
			}
			if p.Name == "" {
				p.Name = strings.TrimSuffix(name, filepath.Ext(name))
			}
			plans = append(plans, p)
		}
	}

	// 3. Process Environment Overlay if requested
	if env != "" {
		for _, envName := range []string{env + ".yaml", env + ".yml", env + ".json"} {
			envPath := filepath.Join(envsDir, envName)
			if b, err := os.ReadFile(envPath); err == nil {
				hashFile(envPath, b)
				var overlay EnvironmentOverlay
				jsonBytes := b
				if !strings.HasSuffix(envName, ".json") {
					j, err := yamlToJSON(b)
					if err != nil {
						return nil, fmt.Errorf("%s: invalid yaml: %w", envPath, err)
					}
					jsonBytes = j
				}
				if err := json.Unmarshal(jsonBytes, &overlay); err != nil {
					return nil, fmt.Errorf("%s: invalid overlay: %w", envPath, err)
				}

				// Apply overlays to APIs
				for i := range apis {
					if u, ok := overlay.UpstreamURLs[apis[i].Name]; ok && u != "" {
						apis[i].UpstreamURL = u
					}
					if l, ok := overlay.Limits[apis[i].Name]; ok && l > 0 {
						apis[i].RateLimitPerMinute = l
					}
				}
				break
			}
		}
	}

	sort.Strings(scannedFiles)
	sourceDigest := hex.EncodeToString(sourceHasher.Sum(nil))

	decl := DeclarativeConfig{
		FormatVersion: DeclarativeFormatVersion,
		APIs:          apis,
		Plans:         plans,
	}

	renderedJSON, err := json.Marshal(decl)
	if err != nil {
		return nil, fmt.Errorf("marshal rendered config: %w", err)
	}
	renderedHash := sha256.Sum256(renderedJSON)

	bundle := &CompiledBundle{
		FormatVersion: "1.0",
		Environment:   env,
		SourceHash:    "sha256:" + sourceDigest,
		RenderedHash:  "sha256:" + hex.EncodeToString(renderedHash[:]),
		Config:        decl,
		Files:         scannedFiles,
	}

	if err := ValidateBundle(bundle); err != nil {
		return nil, err
	}

	return bundle, nil
}

// ValidateBundle ensures the compiled bundle is internally consistent,
// has valid resource paths, no duplicates, and valid format version.
func ValidateBundle(bundle *CompiledBundle) error {
	if bundle == nil {
		return errors.New("nil bundle")
	}
	if bundle.FormatVersion != "1.0" {
		return fmt.Errorf("unsupported bundle format_version %q", bundle.FormatVersion)
	}
	if bundle.Config.FormatVersion != DeclarativeFormatVersion {
		return fmt.Errorf("unsupported declarative config format_version %q", bundle.Config.FormatVersion)
	}

	seenNames := map[string]bool{}
	seenPaths := map[string]string{}
	for i, a := range bundle.Config.APIs {
		if a.Name == "" {
			return fmt.Errorf("apis[%d]: name is required", i)
		}
		if seenNames[a.Name] {
			return fmt.Errorf("apis[%d]: duplicate api name %q", i, a.Name)
		}
		seenNames[a.Name] = true

		if a.BasePath == "" || !strings.HasPrefix(a.BasePath, "/") {
			return fmt.Errorf("apis[%d] %q: base_path must begin with '/'", i, a.Name)
		}
		if other, ok := seenPaths[a.BasePath]; ok {
			return fmt.Errorf("apis[%d] %q: duplicate base_path %q (conflicts with %q)", i, a.Name, a.BasePath, other)
		}
		seenPaths[a.BasePath] = a.Name

		if a.UpstreamURL == "" {
			return fmt.Errorf("apis[%d] %q: upstream_url is required", i, a.Name)
		}
	}

	seenPlans := map[string]bool{}
	for i, p := range bundle.Config.Plans {
		if p.Name == "" {
			return fmt.Errorf("plans[%d]: name is required", i)
		}
		if seenPlans[p.Name] {
			return fmt.Errorf("plans[%d]: duplicate plan name %q", i, p.Name)
		}
		seenPlans[p.Name] = true
	}

	return nil
}

// CanonicalJSON returns an indented, deterministic JSON representation.
func CanonicalJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func yamlToJSON(y []byte) ([]byte, error) {
	var body any
	if err := yaml.Unmarshal(y, &body); err != nil {
		return nil, err
	}
	cleaned := cleanYAMLMap(body)
	return json.Marshal(cleaned)
}

func cleanYAMLMap(i any) any {
	switch x := i.(type) {
	case map[any]any:
		m2 := map[string]any{}
		for k, v := range x {
			m2[fmt.Sprint(k)] = cleanYAMLMap(v)
		}
		return m2
	case map[string]any:
		m2 := map[string]any{}
		for k, v := range x {
			m2[k] = cleanYAMLMap(v)
		}
		return m2
	case []any:
		for idx, v := range x {
			x[idx] = cleanYAMLMap(v)
		}
		return x
	}
	return i
}

