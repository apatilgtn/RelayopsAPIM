package ai

import (
	"errors"
	"fmt"
	"strings"

	"github.com/relayops/apim/internal/store"
)

var (
	ErrNoQualifiedFallback = errors.New("no evaluation-qualified fallback model available for this task class")
	ErrUnpinnedModel       = errors.New("fallback model deployment is not pinned or active")
)

// FallbackRouter selects qualified fallback deployments based on verified release manifests.
type FallbackRouter struct{}

// NewFallbackRouter creates a qualified fallback router.
func NewFallbackRouter() *FallbackRouter {
	return &FallbackRouter{}
}

// SelectFallback finds the highest-priority qualified fallback deployment among allowed models.
func (r *FallbackRouter) SelectFallback(activeManifest store.AIReleaseManifest, fallbackManifests []store.AIReleaseManifest) (*store.AIReleaseManifest, error) {
	if len(fallbackManifests) == 0 {
		return nil, ErrNoQualifiedFallback
	}

	for _, cand := range fallbackManifests {
		// A fallback candidate is eligible ONLY if:
		// 1. It belongs to the same AI Service
		// 2. Its qualification status is 'qualified' or 'active'
		// 3. It possesses a non-empty cryptographic evidence digest
		// 4. It is not the currently failing model deployment
		if cand.ServiceID != activeManifest.ServiceID {
			continue
		}
		if cand.ModelDeploymentID == activeManifest.ModelDeploymentID {
			continue
		}
		if cand.QualificationStatus != "qualified" && cand.QualificationStatus != "active" {
			continue
		}
		if strings.TrimSpace(cand.EvidenceDigest) == "" {
			continue
		}

		// Qualified fallback found!
		return &cand, nil
	}

	return nil, fmt.Errorf("%w: tested %d fallback manifests, none met quality evidence criteria", ErrNoQualifiedFallback, len(fallbackManifests))
}
