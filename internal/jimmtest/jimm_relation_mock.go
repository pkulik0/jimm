// Copyright 2024 Canonical Ltd.

package jimmtest

import (
	"context"

	"github.com/canonical/jimm/v3/internal/errors"
	"github.com/canonical/jimm/v3/internal/openfga"
	apiparams "github.com/canonical/jimm/v3/pkg/api/params"
)

// RelationService is an implementation of the jujuapi.RelationService interface. Every method
// has a corresponding funcion field. Whenever the method is called it
// will delegate to the requested funcion or if the funcion is nil return
// a NotImplemented error.
type RelationService struct {
	AddRelation_            func(ctx context.Context, user *openfga.User, tuples []apiparams.RelationshipTuple) error
	RemoveRelation_         func(ctx context.Context, user *openfga.User, tuples []apiparams.RelationshipTuple) error
	CheckRelation_          func(ctx context.Context, user *openfga.User, tuple apiparams.RelationshipTuple, trace bool) (_ bool, err error)
	ListRelationshipTuples_ func(ctx context.Context, user *openfga.User, tuple apiparams.RelationshipTuple, pageSize int32, continuationToken string) ([]openfga.Tuple, string, error)
}

func (j *JIMM) AddRelation(ctx context.Context, user *openfga.User, tuples []apiparams.RelationshipTuple) error {
	if j.AddRelation_ == nil {
		return errors.E(errors.CodeNotImplemented)
	}
	return j.AddRelation_(ctx, user, tuples)
}

func (j *JIMM) RemoveRelation(ctx context.Context, user *openfga.User, tuples []apiparams.RelationshipTuple) error {
	if j.RemoveRelation_ == nil {
		return errors.E(errors.CodeNotImplemented)
	}
	return j.RemoveRelation_(ctx, user, tuples)
}

func (j *JIMM) CheckRelation(ctx context.Context, user *openfga.User, tuple apiparams.RelationshipTuple, trace bool) (_ bool, err error) {
	if j.CheckRelation_ == nil {
		return false, errors.E(errors.CodeNotImplemented)
	}
	return j.CheckRelation_(ctx, user, tuple, trace)
}

func (j *JIMM) ListRelationshipTuples(ctx context.Context, user *openfga.User, tuple apiparams.RelationshipTuple, pageSize int32, continuationToken string) ([]openfga.Tuple, string, error) {
	if j.ListRelationshipTuples_ == nil {
		return []openfga.Tuple{}, "", errors.E(errors.CodeNotImplemented)
	}
	return j.ListRelationshipTuples_(ctx, user, tuple, pageSize, continuationToken)
}
