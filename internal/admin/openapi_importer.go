package admin

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/relayops/apim/internal/store"
	"gopkg.in/yaml.v3"
)

var slugSanitizer = regexp.MustCompile(`[^a-zA-Z0-9_-]+`)

type OpenAPIImportResult struct {
	API      store.API `json:"api"`
	IsDraft  bool      `json:"is_draft"`
	Warnings []string  `json:"warnings"`
	Summary  string    `json:"summary"`
}

// ParseOpenAPISpec parses an OpenAPI 3.x or Swagger 2.0 document (JSON or YAML)
// and converts it into a validated RelayOps API object saved as a DRAFT for review.
func ParseOpenAPISpec(raw []byte, asDraft bool) (OpenAPIImportResult, error) {
	var doc map[string]any
	// Try JSON first, fallback to YAML
	if err := json.Unmarshal(raw, &doc); err != nil {
		if errYaml := yaml.Unmarshal(raw, &doc); errYaml != nil {
			return OpenAPIImportResult{}, fmt.Errorf("failed to parse spec as JSON or YAML: %w", err)
		}
	}

	info, _ := doc["info"].(map[string]any)
	title, _ := info["title"].(string)
	description, _ := info["description"].(string)
	version, _ := info["version"].(string)

	if title == "" {
		title = "imported-api"
	}
	name := strings.ToLower(slugSanitizer.ReplaceAllString(title, "-"))
	name = strings.Trim(name, "-")
	if name == "" {
		name = "imported-api"
	}

	var warnings []string

	// Validate paths
	paths, hasPaths := doc["paths"].(map[string]any)
	if !hasPaths || len(paths) == 0 {
		return OpenAPIImportResult{}, errors.New("invalid OpenAPI specification: no 'paths' defined")
	}

	// Determine upstream URL & base path
	var upstreamURL string
	var basePath string

	// OpenAPI 3.x servers
	if servers, ok := doc["servers"].([]any); ok && len(servers) > 0 {
		if s0, ok := servers[0].(map[string]any); ok {
			rawURL, _ := s0["url"].(string)
			if u, err := url.Parse(rawURL); err == nil && u.Host != "" {
				upstreamURL = fmt.Sprintf("%s://%s", u.Scheme, u.Host)
				if u.Path != "" && u.Path != "/" {
					basePath = u.Path
				}
			} else if rawURL != "" {
				upstreamURL = rawURL
			}
		}
	}

	// Swagger 2.0 host & basePath
	if upstreamURL == "" {
		host, _ := doc["host"].(string)
		schemes, _ := doc["schemes"].([]any)
		scheme := "https"
		if len(schemes) > 0 {
			if s, ok := schemes[0].(string); ok && s != "" {
				scheme = s
			}
		}
		if host != "" {
			upstreamURL = fmt.Sprintf("%s://%s", scheme, host)
		}
		if bp, _ := doc["basePath"].(string); bp != "" {
			basePath = bp
		}
	}

	if upstreamURL == "" {
		upstreamURL = "http://localhost:7070"
		warnings = append(warnings, "No upstream server URL detected in specification; defaulted to http://localhost:7070")
	}

	if basePath == "" || basePath == "/" {
		basePath = "/" + name
	}
	if !strings.HasPrefix(basePath, "/") {
		basePath = "/" + basePath
	}
	basePath = strings.TrimRight(basePath, "/")

	// Detect Security Schemes
	authType := "none"
	var jwksURL, oidcIssuer, oidcAudience string

	secSchemes := map[string]any{}
	if comp, ok := doc["components"].(map[string]any); ok {
		if ss, ok := comp["securitySchemes"].(map[string]any); ok {
			secSchemes = ss
		}
	}
	if len(secSchemes) == 0 {
		if sd, ok := doc["securityDefinitions"].(map[string]any); ok {
			secSchemes = sd
		}
	}

	for _, v := range secSchemes {
		sm, ok := v.(map[string]any)
		if !ok {
			continue
		}
		stype, _ := sm["type"].(string)
		switch stype {
		case "apiKey":
			authType = "api_key"
		case "oauth2", "openIdConnect":
			authType = "oidc"
			if discoveryURL, ok := sm["openIdConnectUrl"].(string); ok {
				oidcIssuer = discoveryURL
			}
		case "http":
			scheme, _ := sm["scheme"].(string)
			if scheme == "bearer" {
				authType = "jwt"
			}
		}
	}

	if authType == "none" {
		warnings = append(warnings, "No security scheme defined in spec; imported route has auth_type='none'")
	}

	if version != "" && description != "" {
		description = fmt.Sprintf("%s (v%s)", description, version)
	}

	// In safe operations mode, imported specs are saved as DRAFTS by default
	// so operators review and validate before publishing live.
	api := store.API{
		Name:               name,
		Description:        description,
		BasePath:           basePath,
		UpstreamURL:        upstreamURL,
		StripPath:          true,
		AuthType:           authType,
		JWKSURL:            jwksURL,
		OIDCIssuer:         oidcIssuer,
		OIDCAudience:       oidcAudience,
		RateLimitPerMinute: 60,
		QuotaPerDay:        5000,
		QuotaPerMonth:      100000,
		TimeoutMS:          30000,
		CORSEnabled:        true,
		RequestHeaders:     map[string]string{},
		OpenAPISpec:        doc,
		Visibility:         "public",
		RequireApproval:    false,
		IsDraft:            asDraft,
		QuotaFailurePolicy: "fail_open",
		Enabled:            !asDraft, // drafts are disabled until published
	}

	summary := fmt.Sprintf("Imported %d endpoint(s) for %s under base path %s", len(paths), name, basePath)

	return OpenAPIImportResult{
		API:      api,
		IsDraft:  asDraft,
		Warnings: warnings,
		Summary:  summary,
	}, nil
}
