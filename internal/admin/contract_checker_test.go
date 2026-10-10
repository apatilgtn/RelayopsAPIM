package admin

import (
	"testing"
)

func TestContractChecker_RefResolutionAndBreakingChanges(t *testing.T) {
	baseSpec := `
openapi: 3.0.0
info:
  title: Sample API
  version: 1.0.0
paths:
  /orders:
    post:
      parameters:
        - name: status
          in: query
          required: false
          schema:
            type: string
            enum: ["pending", "shipped", "delivered"]
      requestBody:
        required: true
        content:
          application/json:
            schema:
              $ref: "#/components/schemas/OrderInput"
      responses:
        "200":
          description: Order created
          content:
            application/json:
              schema:
                $ref: "#/components/schemas/OrderOutput"
components:
  schemas:
    OrderInput:
      type: object
      required:
        - item_id
      properties:
        item_id:
          type: string
        quantity:
          type: integer
        shipping_address:
          $ref: "#/components/schemas/Address"
    Address:
      type: object
      properties:
        city:
          type: string
        zipcode:
          type: string
    OrderOutput:
      type: object
      required:
        - order_id
        - status
      properties:
        order_id:
          type: string
        status:
          type: string
`

	// Proposed spec has 5 breaking changes:
	// 1. Parameter enum option "delivered" removed
	// 2. Newly required request property "quantity"
	// 3. Nested property "zipcode" changed type from string to integer
	// 4. Response property "status" removed from required list / response
	// 5. Parameter "priority" is newly required
	proposedSpec := `
openapi: 3.0.0
info:
  title: Sample API
  version: 1.1.0
paths:
  /orders:
    post:
      parameters:
        - name: status
          in: query
          required: false
          schema:
            type: string
            enum: ["pending", "shipped"]
        - name: priority
          in: query
          required: true
          schema:
            type: string
      requestBody:
        required: true
        content:
          application/json:
            schema:
              $ref: "#/components/schemas/OrderInput"
      responses:
        "200":
          description: Order created
          content:
            application/json:
              schema:
                $ref: "#/components/schemas/OrderOutput"
components:
  schemas:
    OrderInput:
      type: object
      required:
        - item_id
        - quantity
      properties:
        item_id:
          type: string
        quantity:
          type: integer
        shipping_address:
          $ref: "#/components/schemas/Address"
    Address:
      type: object
      properties:
        city:
          type: string
        zipcode:
          type: integer
    OrderOutput:
      type: object
      required:
        - order_id
      properties:
        order_id:
          type: string
`

	baseDoc, err := ParseSpec([]byte(baseSpec))
	if err != nil {
		t.Fatalf("failed to parse base spec: %v", err)
	}
	propDoc, err := ParseSpec([]byte(proposedSpec))
	if err != nil {
		t.Fatalf("failed to parse proposed spec: %v", err)
	}

	report := CompareOpenAPISpecs(baseDoc, propDoc)

	if report.IsCompatible {
		t.Errorf("expected incompatible report, got compatible")
	}

	if report.BreakingChangesCount < 4 {
		t.Errorf("expected at least 4 breaking changes, got %d", report.BreakingChangesCount)
	}

	// Verify specific breaking changes were detected
	foundEnumRemoval := false
	foundNewRequiredParam := false
	foundNewRequiredBodyProp := false
	foundTypeMutation := false
	foundResponseRemoval := false

	for _, d := range report.Differences {
		t.Logf("Detected diff: [%s] %s: %s", d.Severity, d.Type, d.Description)
		switch {
		case d.Type == ChangeIncompatibleSchema && (d.Description != "" && (contains(d.Description, "enum") || contains(d.Description, "delivered"))):
			foundEnumRemoval = true
		case d.Type == ChangeNewRequiredParam && contains(d.Description, "priority"):
			foundNewRequiredParam = true
		case d.Type == ChangeIncompatibleSchema && contains(d.Description, "quantity"):
			foundNewRequiredBodyProp = true
		case d.Type == ChangeIncompatibleSchema && contains(d.Description, "zipcode"):
			foundTypeMutation = true
		case d.Type == ChangeIncompatibleSchema && contains(d.Description, "status"):
			foundResponseRemoval = true
		}
	}

	if !foundEnumRemoval {
		t.Errorf("expected enum removal detection for 'delivered'")
	}
	if !foundNewRequiredParam {
		t.Errorf("expected new required param detection for 'priority'")
	}
	if !foundNewRequiredBodyProp {
		t.Errorf("expected new required body property detection for 'quantity'")
	}
	if !foundTypeMutation {
		t.Errorf("expected type mutation detection for 'zipcode'")
	}
	if !foundResponseRemoval {
		t.Errorf("expected response property removal detection for 'status'")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 || (len(s) > 0 && len(substr) > 0 && (s != "" && (indexOf(s, substr) >= 0))))
}

func indexOf(s, substr string) int {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}
