package graphql

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// SchemaDiff represents breaking and non-breaking alterations between two GraphQL SDL schemas.
type SchemaDiff struct {
	HasBreaking     bool     `json:"has_breaking"`
	BreakingChanges []string `json:"breaking_changes"`
	SafeChanges     []string `json:"safe_changes"`
}

// ParsedType represents a GraphQL type with its fields.
type ParsedType struct {
	Name   string
	Kind   string            // type, interface, input, enum
	Fields map[string]string // fieldName -> fieldType
}

// DiffSchemas compares baseline SDL vs candidate SDL to identify breaking changes.
func DiffSchemas(baselineSDL, candidateSDL string) SchemaDiff {
	diff := SchemaDiff{
		HasBreaking:     false,
		BreakingChanges: []string{},
		SafeChanges:     []string{},
	}

	baseTypes := parseSDL(baselineSDL)
	candTypes := parseSDL(candidateSDL)

	// Check for removed types or altered types
	for typeName, baseT := range baseTypes {
		candT, exists := candTypes[typeName]
		if !exists {
			diff.BreakingChanges = append(diff.BreakingChanges, fmt.Sprintf("Type '%s' was removed from schema", typeName))
			continue
		}

		if baseT.Kind != candT.Kind {
			diff.BreakingChanges = append(diff.BreakingChanges, fmt.Sprintf("Type '%s' kind changed from %s to %s", typeName, baseT.Kind, candT.Kind))
			continue
		}

		// Check fields
		for fName, fType := range baseT.Fields {
			candFType, fExists := candT.Fields[fName]
			if !fExists {
				diff.BreakingChanges = append(diff.BreakingChanges, fmt.Sprintf("Field '%s.%s' was removed", typeName, fName))
				continue
			}

			// Check type compatibility
			cleanBase := strings.TrimRight(fType, "!")
			cleanCand := strings.TrimRight(candFType, "!")
			if cleanBase != cleanCand {
				diff.BreakingChanges = append(diff.BreakingChanges, fmt.Sprintf("Field '%s.%s' type changed from %s to %s", typeName, fName, fType, candFType))
			} else if strings.HasSuffix(candFType, "!") && !strings.HasSuffix(fType, "!") {
				// Turning optional return field to required return field is safe in output types, but breaking in input types
				if baseT.Kind == "input" {
					diff.BreakingChanges = append(diff.BreakingChanges, fmt.Sprintf("Input field '%s.%s' made non-nullable (%s)", typeName, fName, candFType))
				} else {
					diff.SafeChanges = append(diff.SafeChanges, fmt.Sprintf("Output field '%s.%s' tightened nullability to non-null", typeName, fName))
				}
			}
		}

		// Check for added fields in candidate
		for candFName := range candT.Fields {
			if _, exists := baseT.Fields[candFName]; !exists {
				diff.SafeChanges = append(diff.SafeChanges, fmt.Sprintf("Field '%s.%s' was added", typeName, candFName))
			}
		}
	}

	// Check for new types in candidate
	for candTypeName := range candTypes {
		if _, exists := baseTypes[candTypeName]; !exists {
			diff.SafeChanges = append(diff.SafeChanges, fmt.Sprintf("Type '%s' was added to schema", candTypeName))
		}
	}

	sort.Strings(diff.BreakingChanges)
	sort.Strings(diff.SafeChanges)
	diff.HasBreaking = len(diff.BreakingChanges) > 0
	return diff
}

// parseSDL is a lightweight SDL extractor for types, inputs, interfaces, and enums.
func parseSDL(sdl string) map[string]ParsedType {
	types := make(map[string]ParsedType)
	lines := strings.Split(sdl, "\n")

	typeStartRegex := regexp.MustCompile(`^\s*(type|input|interface|enum)\s+([a-zA-Z0-9_]+)`)
	fieldRegex := regexp.MustCompile(`^\s*([a-zA-Z0-9_]+)\s*(?:\([^)]*\))?\s*:\s*([a-zA-Z0-9_!\[\]]+)`)
	enumValRegex := regexp.MustCompile(`^\s*([a-zA-Z0-9_]+)\s*$`)

	var currentType *ParsedType

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") || trimmed == "" {
			continue
		}

		if match := typeStartRegex.FindStringSubmatch(trimmed); len(match) > 2 {
			kind := match[1]
			name := match[2]
			t := ParsedType{
				Name:   name,
				Kind:   kind,
				Fields: make(map[string]string),
			}
			types[name] = t
			currentType = &t
			continue
		}

		if strings.Contains(trimmed, "}") {
			if currentType != nil {
				types[currentType.Name] = *currentType
			}
			currentType = nil
			continue
		}

		if currentType != nil {
			if currentType.Kind == "enum" {
				if match := enumValRegex.FindStringSubmatch(trimmed); len(match) > 1 {
					currentType.Fields[match[1]] = "enum_value"
				}
			} else {
				if match := fieldRegex.FindStringSubmatch(trimmed); len(match) > 2 {
					fName := match[1]
					fType := match[2]
					currentType.Fields[fName] = fType
				}
			}
		}
	}

	return types
}
