// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package controlapi

import (
	"context"
	"errors"
	"fmt"

	"github.com/agent-substrate/substrate/cmd/ateapi/internal/store"
	"github.com/agent-substrate/substrate/internal/resources"
	"github.com/agent-substrate/substrate/internal/volume"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

// PurgeActorVolumes permanently removes the backing storage of an actor's
// external volumes. This is distinct from DeleteActor, whose volume deletion
// is a no-op for sticky volumes so that a delete+recreate refresh re-attaches
// the same data: purging is for user/account removal, when the data must go.
//
// Volume IDs are derived from the template's external volume templates via
// actorVolumeID, so this works even after the actor record itself is deleted.
// The purge is refused while the actor still exists and is RUNNING, since its
// volumes may still be mounted on a worker node.
func (s *Service) PurgeActorVolumes(ctx context.Context, req *ateapipb.PurgeActorVolumesRequest) (*ateapipb.PurgeActorVolumesResponse, error) {
	if err := validatePurgeActorVolumesRequest(req); err != nil {
		return nil, err
	}
	setSpanActorRefAttributes(ctx, req.GetActor().GetAtespace(), req.GetActor().GetName())

	atespace := req.GetActor().GetAtespace()
	name := req.GetActor().GetName()

	// Refuse to purge while the actor is still around and RUNNING: its
	// volumes may be mounted, and deleting data under a live workload is
	// never what the caller wants. A SUSPENDED or non-existent actor is fine
	// (the common case is a delete-first-then-purge flow).
	actor, err := s.persistence.GetActor(ctx, atespace, name)
	switch {
	case err == nil:
		if actor.GetStatus() == ateapipb.Actor_STATUS_RUNNING {
			return nil, status.Errorf(codes.FailedPrecondition,
				"actor %s is RUNNING; suspend or delete it before purging its volumes", name)
		}
	case errors.Is(err, store.ErrNotFound):
		// Actor already gone: the post-delete purge case. Proceed.
	default:
		return nil, fmt.Errorf("while fetching actor: %w", err)
	}

	template, err := s.actorTemplateLister.ActorTemplates(req.GetTemplate().GetAtespace()).Get(req.GetTemplate().GetName())
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "ActorTemplate %s/%s not found", req.GetTemplate().GetAtespace(), req.GetTemplate().GetName())
	}

	purger, ok := getVolumePlugin().(volume.VolumePurger)
	if !ok {
		return nil, status.Errorf(codes.FailedPrecondition,
			"volume plugin %T does not support purging volumes", getVolumePlugin())
	}

	ref := &ateapipb.ObjectRef{Atespace: atespace, Name: name}
	var purged []string
	var errs []error
	for _, vol := range template.Spec.Volumes {
		if vol.ExternalVolumeTemplate == nil {
			continue
		}
		volumeID := actorVolumeID(ref, vol.Name)
		if err := purger.PurgeVolume(ctx, volumeID); err != nil {
			errs = append(errs, fmt.Errorf("failed to purge volume %q: %w", volumeID, err))
			continue
		}
		purged = append(purged, volumeID)
	}
	if len(errs) > 0 {
		return nil, status.Errorf(codes.Internal, "while purging actor volumes: %v", errors.Join(errs...))
	}

	return &ateapipb.PurgeActorVolumesResponse{PurgedVolumeIds: purged}, nil
}

func validatePurgeActorVolumesRequest(req *ateapipb.PurgeActorVolumesRequest) error {
	var fldPath *field.Path
	var errs field.ErrorList

	if val, fldPath := req.Actor, fldPath.Child("actor"); val == nil {
		errs = append(errs, field.Required(fldPath, ""))
	} else {
		errs = append(errs, resources.ValidateObjectRef(val, fldPath)...)
	}
	if val, fldPath := req.Template, fldPath.Child("template"); val == nil {
		errs = append(errs, field.Required(fldPath, ""))
	} else {
		errs = append(errs, resources.ValidateObjectRef(val, fldPath)...)
	}

	if len(errs) > 0 {
		return status.Error(codes.InvalidArgument, errs.ToAggregate().Error())
	}
	return nil
}
