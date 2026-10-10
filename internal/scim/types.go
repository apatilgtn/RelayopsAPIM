package scim

import "time"

const (
	UserSchema                  = "urn:ietf:params:scim:schemas:core:2.0:User"
	ListResponseSchema          = "urn:ietf:params:scim:api:messages:2.0:ListResponse"
	ErrorSchema                 = "urn:ietf:params:scim:api:messages:2.0:Error"
	PatchOpSchema               = "urn:ietf:params:scim:api:messages:2.0:PatchOp"
	ServiceProviderConfigSchema = "urn:ietf:params:scim:schemas:core:2.0:ServiceProviderConfig"
	ResourceTypeSchema          = "urn:ietf:params:scim:schemas:core:2.0:ResourceType"
)

type UserResource struct {
	Schemas  []string    `json:"schemas"`
	ID       string      `json:"id"`
	UserName string      `json:"userName"`
	Name     UserName    `json:"name,omitempty"`
	Emails   []UserEmail `json:"emails,omitempty"`
	Roles    []UserRole  `json:"roles,omitempty"`
	Active   bool        `json:"active"`
	Meta     Meta        `json:"meta"`
}

type UserName struct {
	Formatted  string `json:"formatted,omitempty"`
	FamilyName string `json:"familyName,omitempty"`
	GivenName  string `json:"givenName,omitempty"`
}

type UserEmail struct {
	Value   string `json:"value"`
	Type    string `json:"type,omitempty"`
	Primary bool   `json:"primary,omitempty"`
}

type UserRole struct {
	Value   string `json:"value"`
	Primary bool   `json:"primary,omitempty"`
}

type Meta struct {
	ResourceType string    `json:"resourceType"`
	Created      time.Time `json:"created,omitempty"`
	LastModified time.Time `json:"lastModified,omitempty"`
	Location     string    `json:"location,omitempty"`
}

type ListResponse struct {
	Schemas      []string       `json:"schemas"`
	TotalResults int            `json:"totalResults"`
	StartIndex   int            `json:"startIndex"`
	ItemsPerPage int            `json:"itemsPerPage"`
	Resources    []UserResource `json:"Resources"`
}

type ErrorResponse struct {
	Schemas  []string `json:"schemas"`
	Status   string   `json:"status"`
	Detail   string   `json:"detail"`
	ScimType string   `json:"scimType,omitempty"`
}

type PatchOperation struct {
	Op    string `json:"op"`
	Path  string `json:"path,omitempty"`
	Value any    `json:"value"`
}

type PatchRequest struct {
	Schemas    []string         `json:"schemas"`
	Operations []PatchOperation `json:"Operations"`
}

type ServiceProviderConfig struct {
	Schemas               []string         `json:"schemas"`
	DocumentationURI      string           `json:"documentationUri,omitempty"`
	Patch                 SupportedFeature `json:"patch"`
	Bulk                  SupportedFeature `json:"bulk"`
	Filter                FilterFeature    `json:"filter"`
	ChangePassword        SupportedFeature `json:"changePassword"`
	Sort                  SupportedFeature `json:"sort"`
	Etag                  SupportedFeature `json:"etag"`
	AuthenticationSchemes []AuthScheme     `json:"authenticationSchemes"`
}

type SupportedFeature struct {
	Supported bool `json:"supported"`
}

type FilterFeature struct {
	Supported  bool `json:"supported"`
	MaxResults int  `json:"maxResults"`
}

type AuthScheme struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	SpecURI     string `json:"specUri,omitempty"`
	Type        string `json:"type"`
	Primary     bool   `json:"primary,omitempty"`
}
