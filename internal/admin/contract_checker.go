package admin

import (
	"encoding/json"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

type ContractChangeType string

const (
	ChangeRemovedEndpoint    ContractChangeType = "REMOVED_ENDPOINT"
	ChangeRemovedOperation   ContractChangeType = "REMOVED_OPERATION"
	ChangeNewRequiredParam   ContractChangeType = "NEW_REQUIRED_PARAM"
	ChangeIncompatibleSchema ContractChangeType = "INCOMPATIBLE_SCHEMA"
	ChangeRemovedResponse    ContractChangeType = "REMOVED_RESPONSE"
	ChangeInfo               ContractChangeType = "NON_BREAKING_CHANGE"
)

type ContractDifference struct {
	Type        ContractChangeType `json:"type"`
	Severity    string             `json:"severity"` // CRITICAL_BREAKING, HIGH_BREAKING, WARNING, INFO
	Path        string             `json:"path"`
	Method      string             `json:"method,omitempty"`
	Description string             `json:"description"`
	Remediation string             `json:"remediation"`
}

type OpenAPIContractReport struct {
	IsCompatible         bool                 `json:"is_compatible"`
	BreakingChangesCount int                  `json:"breaking_changes_count"`
	TotalChangesCount    int                  `json:"total_changes_count"`
	Differences          []ContractDifference `json:"differences"`
	Summary              string               `json:"summary"`
}

// ParseSpec parses raw JSON or YAML bytes into a map
func ParseSpec(raw []byte) (map[string]any, error) {
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		if errYaml := yaml.Unmarshal(raw, &doc); errYaml != nil {
			return nil, fmt.Errorf("failed to parse spec as JSON or YAML: %w", err)
		}
	}
	return doc, nil
}

// resolveRef navigates a JSON reference like "#/components/schemas/Pet" in the document.
func resolveRef(doc map[string]any, ref string, visited map[string]bool) (map[string]any, bool) {
	if !strings.HasPrefix(ref, "#/") {
		return nil, false
	}
	if visited == nil {
		visited = make(map[string]bool)
	}
	if visited[ref] {
		return nil, false // circular reference guard
	}
	visited[ref] = true

	parts := strings.Split(strings.TrimPrefix(ref, "#/"), "/")
	var current any = doc
	for _, part := range parts {
		// unescape ~1 and ~0 per RFC 6901
		part = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
		m, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		val, exists := m[part]
		if !exists {
			return nil, false
		}
		current = val
	}
	resolved, ok := current.(map[string]any)
	if !ok {
		return nil, false
	}
	// Check if the resolved node is itself a reference
	if nextRef, hasRef := resolved["$ref"].(string); hasRef && nextRef != "" {
		return resolveRef(doc, nextRef, visited)
	}
	return resolved, true
}

func derefSchema(doc, schema map[string]any) map[string]any {
	if schema == nil {
		return nil
	}
	if ref, ok := schema["$ref"].(string); ok && ref != "" {
		if resolved, ok := resolveRef(doc, ref, nil); ok {
			return resolved
		}
	}
	return schema
}

// CompareOpenAPISpecs compares base vs proposed OpenAPI specs and identifies breaking changes
func CompareOpenAPISpecs(base, proposed map[string]any) OpenAPIContractReport {
	var diffs []ContractDifference

	basePaths, _ := base["paths"].(map[string]any)
	proposedPaths, _ := proposed["paths"].(map[string]any)

	if basePaths == nil {
		basePaths = make(map[string]any)
	}
	if proposedPaths == nil {
		proposedPaths = make(map[string]any)
	}

	httpMethods := []string{"get", "post", "put", "delete", "patch", "options", "head"}

	// 1. Check for removed endpoints and removed operations
	for path, baseItem := range basePaths {
		basePathMap, ok := baseItem.(map[string]any)
		if !ok {
			continue
		}

		propItem, pathExists := proposedPaths[path]
		if !pathExists {
			diffs = append(diffs, ContractDifference{
				Type:        ChangeRemovedEndpoint,
				Severity:    "CRITICAL_BREAKING",
				Path:        path,
				Description: fmt.Sprintf("Path '%s' was completely removed from the OpenAPI specification.", path),
				Remediation: "Ensure all client applications migrate away from this endpoint or provide URL rewrites.",
			})
			continue
		}

		propPathMap, ok := propItem.(map[string]any)
		if !ok {
			continue
		}

		for _, m := range httpMethods {
			baseOp, baseHasMethod := basePathMap[m].(map[string]any)
			propOp, propHasMethod := propPathMap[m].(map[string]any)

			methodUpper := strings.ToUpper(m)

			if baseHasMethod && !propHasMethod {
				diffs = append(diffs, ContractDifference{
					Type:        ChangeRemovedOperation,
					Severity:    "CRITICAL_BREAKING",
					Path:        path,
					Method:      methodUpper,
					Description: fmt.Sprintf("Operation '%s %s' was removed from the specification.", methodUpper, path),
					Remediation: "Clients invoking this HTTP verb will receive HTTP 405 Method Not Allowed.",
				})
				continue
			}

			if baseHasMethod && propHasMethod {
				// Compare parameters with $ref resolution, type changes and enum value removals
				baseParams := extractParamsWithRef(base, baseOp)
				propParams := extractParamsWithRef(proposed, propOp)

				for pKey, propP := range propParams {
					baseP, existed := baseParams[pKey]
					isReq := propP.Required
					if isReq && (!existed || !baseP.Required) {
						diffs = append(diffs, ContractDifference{
							Type:        ChangeNewRequiredParam,
							Severity:    "HIGH_BREAKING",
							Path:        path,
							Method:      methodUpper,
							Description: fmt.Sprintf("Parameter '%s' (in: %s) is now required on '%s %s'.", propP.Name, propP.In, methodUpper, path),
							Remediation: "Clients not supplying this parameter will fail validation.",
						})
					}

					if existed {
						// Check parameter type change
						if baseP.Type != "" && propP.Type != "" && baseP.Type != propP.Type {
							diffs = append(diffs, ContractDifference{
								Type:        ChangeIncompatibleSchema,
								Severity:    "HIGH_BREAKING",
								Path:        path,
								Method:      methodUpper,
								Description: fmt.Sprintf("Parameter '%s' (in: %s) changed type from '%s' to '%s' on '%s %s'.", propP.Name, propP.In, baseP.Type, propP.Type, methodUpper, path),
								Remediation: "Update caller applications to provide the expected parameter type.",
							})
						}
						// Check enum value removal
						if len(baseP.Enum) > 0 {
							propEnumSet := make(map[string]bool)
							for _, e := range propP.Enum {
								propEnumSet[fmt.Sprint(e)] = true
							}
							for _, e := range baseP.Enum {
								es := fmt.Sprint(e)
								if !propEnumSet[es] {
									diffs = append(diffs, ContractDifference{
										Type:        ChangeIncompatibleSchema,
										Severity:    "HIGH_BREAKING",
										Path:        path,
										Method:      methodUpper,
										Description: fmt.Sprintf("Parameter '%s' (in: %s) removed supported enum option '%s' on '%s %s'.", propP.Name, propP.In, es, methodUpper, path),
										Remediation: "Callers sending this previously valid enum value will now be rejected.",
									})
								}
							}
						}
					}
				}

				// Compare requestBody schema recursively (resolving $ref)
				baseReqSchema := extractBodySchemaWithRef(base, baseOp)
				propReqSchema := extractBodySchemaWithRef(proposed, propOp)
				if baseReqSchema != nil || propReqSchema != nil {
					schemaDiffs := compareSchemas(base, proposed, path, methodUpper, "Request body", baseReqSchema, propReqSchema, true, make(map[string]bool))
					diffs = append(diffs, schemaDiffs...)
				}

				// Compare responses and response schemas
				baseResponses, _ := baseOp["responses"].(map[string]any)
				propResponses, _ := propOp["responses"].(map[string]any)
				if baseResponses != nil && propResponses != nil {
					for status, baseRespVal := range baseResponses {
						propRespVal, exists := propResponses[status]
						if !exists && (strings.HasPrefix(status, "2") || status == "default") {
							diffs = append(diffs, ContractDifference{
								Type:        ChangeRemovedResponse,
								Severity:    "WARNING",
								Path:        path,
								Method:      methodUpper,
								Description: fmt.Sprintf("Successful response status code '%s' was removed from '%s %s'.", status, methodUpper, path),
								Remediation: "Verify whether clients explicitly expect this HTTP response code.",
							})
							continue
						}

						// If response exists and is 2xx, compare response payload schemas
						if exists && strings.HasPrefix(status, "2") {
							baseRespSchema := extractResponseSchemaWithRef(base, baseRespVal)
							propRespSchema := extractResponseSchemaWithRef(proposed, propRespVal)
							if baseRespSchema != nil && propRespSchema != nil {
								respDiffs := compareSchemas(base, proposed, path, methodUpper, fmt.Sprintf("Response %s", status), baseRespSchema, propRespSchema, false, make(map[string]bool))
								diffs = append(diffs, respDiffs...)
							}
						}
					}
				}
			}
		}
	}

	breakingCount := 0
	for _, d := range diffs {
		if d.Severity == "CRITICAL_BREAKING" || d.Severity == "HIGH_BREAKING" {
			breakingCount++
		}
	}

	isCompat := breakingCount == 0
	summary := fmt.Sprintf("Contract Check Passed: %d non-breaking differences identified.", len(diffs))
	if !isCompat {
		summary = fmt.Sprintf("Contract Check Failed: %d breaking change(s) detected across %d total difference(s).", breakingCount, len(diffs))
	}

	return OpenAPIContractReport{
		IsCompatible:         isCompat,
		BreakingChangesCount: breakingCount,
		TotalChangesCount:    len(diffs),
		Differences:          diffs,
		Summary:              summary,
	}
}

type fullParamInfo struct {
	Name     string
	In       string
	Type     string
	Required bool
	Enum     []any
}

func extractParamsWithRef(doc, op map[string]any) map[string]fullParamInfo {
	out := make(map[string]fullParamInfo)
	paramsList, ok := op["parameters"].([]any)
	if !ok {
		return out
	}
	for _, item := range paramsList {
		pMap, ok := item.(map[string]any)
		if !ok {
			continue
		}
		// Resolve $ref if present
		if ref, ok := pMap["$ref"].(string); ok && ref != "" {
			if resolved, ok := resolveRef(doc, ref, nil); ok {
				pMap = resolved
			}
		}
		name, _ := pMap["name"].(string)
		in, _ := pMap["in"].(string)
		req, _ := pMap["required"].(bool)
		pType := ""
		var enum []any

		// OpenAPI 3.x schema or Swagger 2.x type
		if sMap, ok := pMap["schema"].(map[string]any); ok {
			sMap = derefSchema(doc, sMap)
			if t, ok := sMap["type"].(string); ok {
				pType = t
			}
			if e, ok := sMap["enum"].([]any); ok {
				enum = e
			}
		} else {
			if t, ok := pMap["type"].(string); ok {
				pType = t
			}
			if e, ok := pMap["enum"].([]any); ok {
				enum = e
			}
		}

		if name != "" {
			out[in+":"+name] = fullParamInfo{
				Name:     name,
				In:       in,
				Type:     pType,
				Required: req,
				Enum:     enum,
			}
		}
	}
	return out
}

func extractBodySchemaWithRef(doc, op map[string]any) map[string]any {
	rb, ok := op["requestBody"].(map[string]any)
	if !ok {
		return nil
	}
	if ref, ok := rb["$ref"].(string); ok && ref != "" {
		if resolved, ok := resolveRef(doc, ref, nil); ok {
			rb = resolved
		}
	}
	content, ok := rb["content"].(map[string]any)
	if !ok {
		return nil
	}
	for _, mime := range []string{"application/json", "application/*+json", "*/*"} {
		if entry, ok := content[mime].(map[string]any); ok {
			if schema, ok := entry["schema"].(map[string]any); ok {
				return derefSchema(doc, schema)
			}
		}
	}
	return nil
}

func extractResponseSchemaWithRef(doc map[string]any, respVal any) map[string]any {
	respMap, ok := respVal.(map[string]any)
	if !ok {
		return nil
	}
	if ref, ok := respMap["$ref"].(string); ok && ref != "" {
		if resolved, ok := resolveRef(doc, ref, nil); ok {
			respMap = resolved
		}
	}
	content, ok := respMap["content"].(map[string]any)
	if !ok {
		// Swagger 2.x schema directly on response
		if schema, ok := respMap["schema"].(map[string]any); ok {
			return derefSchema(doc, schema)
		}
		return nil
	}
	for _, mime := range []string{"application/json", "application/*+json", "*/*"} {
		if entry, ok := content[mime].(map[string]any); ok {
			if schema, ok := entry["schema"].(map[string]any); ok {
				return derefSchema(doc, schema)
			}
		}
	}
	return nil
}

// compareSchemas recursively compares two schemas (resolving $ref, types, enums, required fields, and nested properties)
func compareSchemas(baseDoc, propDoc map[string]any, path, method, location string, baseSchema, propSchema map[string]any, isInput bool, visited map[string]bool) []ContractDifference {
	var diffs []ContractDifference

	if baseSchema == nil && propSchema == nil {
		return diffs
	}

	baseSchema = derefSchema(baseDoc, baseSchema)
	propSchema = derefSchema(propDoc, propSchema)

	if baseSchema == nil && propSchema != nil {
		if isInput {
			// Newly introduced request body or property
			if reqList, ok := propSchema["required"].([]any); ok && len(reqList) > 0 {
				diffs = append(diffs, ContractDifference{
					Type:        ChangeIncompatibleSchema,
					Severity:    "HIGH_BREAKING",
					Path:        path,
					Method:      method,
					Description: fmt.Sprintf("%s was newly introduced with required fields on '%s %s'.", location, method, path),
					Remediation: "Ensure existing clients provide all required payload fields.",
				})
			}
		}
		return diffs
	}

	if baseSchema != nil && propSchema == nil {
		if !isInput {
			// Response payload removed
			diffs = append(diffs, ContractDifference{
				Type:        ChangeIncompatibleSchema,
				Severity:    "HIGH_BREAKING",
				Path:        path,
				Method:      method,
				Description: fmt.Sprintf("%s payload schema was removed on '%s %s'.", location, method, path),
				Remediation: "Verify whether clients parse response payloads for this operation.",
			})
		}
		return diffs
	}

	// 1. Compare type mutations
	baseType, _ := baseSchema["type"].(string)
	propType, _ := propSchema["type"].(string)
	if baseType != "" && propType != "" && baseType != propType {
		diffs = append(diffs, ContractDifference{
			Type:        ChangeIncompatibleSchema,
			Severity:    "HIGH_BREAKING",
			Path:        path,
			Method:      method,
			Description: fmt.Sprintf("%s schema changed type from '%s' to '%s' on '%s %s'.", location, baseType, propType, method, path),
			Remediation: "Client applications expecting previous data format will fail type decoding.",
		})
	}

	// 2. Compare enum options
	baseEnums, hasBaseEnums := baseSchema["enum"].([]any)
	propEnums, hasPropEnums := propSchema["enum"].([]any)
	if hasBaseEnums && hasPropEnums && isInput {
		propSet := make(map[string]bool)
		for _, e := range propEnums {
			propSet[fmt.Sprint(e)] = true
		}
		for _, e := range baseEnums {
			es := fmt.Sprint(e)
			if !propSet[es] {
				diffs = append(diffs, ContractDifference{
					Type:        ChangeIncompatibleSchema,
					Severity:    "HIGH_BREAKING",
					Path:        path,
					Method:      method,
					Description: fmt.Sprintf("%s schema removed supported enum value '%s' on '%s %s'.", location, es, method, path),
					Remediation: "Callers sending this enum value will fail schema validation.",
				})
			}
		}
	}

	// 3. Compare required properties
	baseReq := make(map[string]bool)
	if reqList, ok := baseSchema["required"].([]any); ok {
		for _, r := range reqList {
			if s, ok := r.(string); ok {
				baseReq[s] = true
			}
		}
	}
	propReq := make(map[string]bool)
	if reqList, ok := propSchema["required"].([]any); ok {
		for _, r := range reqList {
			if s, ok := r.(string); ok {
				propReq[s] = true
			}
		}
	}

	if isInput {
		// Newly required input properties are breaking for callers
		for propKey := range propReq {
			if !baseReq[propKey] {
				diffs = append(diffs, ContractDifference{
					Type:        ChangeIncompatibleSchema,
					Severity:    "HIGH_BREAKING",
					Path:        path,
					Method:      method,
					Description: fmt.Sprintf("%s introduced new required property '%s' on '%s %s'.", location, propKey, method, path),
					Remediation: "Ensure client request payloads include this newly required field.",
				})
			}
		}
	} else {
		// Output: removed required properties from response schema are breaking for clients
		for propKey := range baseReq {
			if !propReq[propKey] {
				diffs = append(diffs, ContractDifference{
					Type:        ChangeIncompatibleSchema,
					Severity:    "HIGH_BREAKING",
					Path:        path,
					Method:      method,
					Description: fmt.Sprintf("%s removed previously guaranteed required property '%s' on '%s %s'.", location, propKey, method, path),
					Remediation: "Client deserializers expecting this field in the response may fail.",
				})
			}
		}
	}

	// 4. Compare properties recursively
	baseProps, _ := baseSchema["properties"].(map[string]any)
	propProps, _ := propSchema["properties"].(map[string]any)
	if baseProps != nil || propProps != nil {
		if !isInput && baseProps != nil {
			// Check if any property was completely removed from response payload
			for propKey := range baseProps {
				if propProps == nil || propProps[propKey] == nil {
					diffs = append(diffs, ContractDifference{
						Type:        ChangeIncompatibleSchema,
						Severity:    "HIGH_BREAKING",
						Path:        path,
						Method:      method,
						Description: fmt.Sprintf("%s removed property '%s' from response payload on '%s %s'.", location, propKey, method, path),
						Remediation: "Clients consuming this response attribute will encounter missing data.",
					})
				}
			}
		}

		// Recursively compare common properties
		for propKey, basePropVal := range baseProps {
			if propProps != nil && propProps[propKey] != nil {
				bpMap, ok1 := basePropVal.(map[string]any)
				ppMap, ok2 := propProps[propKey].(map[string]any)
				if ok1 && ok2 {
					subDiffs := compareSchemas(baseDoc, propDoc, path, method, fmt.Sprintf("%s property '%s'", location, propKey), bpMap, ppMap, isInput, visited)
					diffs = append(diffs, subDiffs...)
				}
			}
		}
	}

	// 5. Compare array items recursively
	baseItems, ok1 := baseSchema["items"].(map[string]any)
	propItems, ok2 := propSchema["items"].(map[string]any)
	if ok1 && ok2 {
		itemDiffs := compareSchemas(baseDoc, propDoc, path, method, fmt.Sprintf("%s items", location), baseItems, propItems, isInput, visited)
		diffs = append(diffs, itemDiffs...)
	}

	return diffs
}
