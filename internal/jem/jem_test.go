// Copyright 2015 Canonical Ltd.

package jem_test

import (
	"fmt"
	"time"

	cloudapi "github.com/juju/juju/api/cloud"
	"github.com/juju/juju/api/controller"
	modelmanagerapi "github.com/juju/juju/api/modelmanager"
	jujuparams "github.com/juju/juju/apiserver/params"
	"github.com/juju/juju/state/multiwatcher"
	jujujujutesting "github.com/juju/juju/testing"
	jt "github.com/juju/testing"
	jc "github.com/juju/testing/checkers"
	"golang.org/x/net/context"
	gc "gopkg.in/check.v1"
	"gopkg.in/errgo.v1"
	"gopkg.in/juju/names.v2"
	"gopkg.in/mgo.v2/bson"

	"github.com/CanonicalLtd/jem/internal/apiconn"
	"github.com/CanonicalLtd/jem/internal/auth"
	"github.com/CanonicalLtd/jem/internal/jem"
	"github.com/CanonicalLtd/jem/internal/jemtest"
	"github.com/CanonicalLtd/jem/internal/mgosession"
	"github.com/CanonicalLtd/jem/internal/mongodoc"
	"github.com/CanonicalLtd/jem/params"
)

type jemSuite struct {
	jemtest.JujuConnSuite
	pool        *jem.Pool
	sessionPool *mgosession.Pool
	jem         *jem.JEM
}

var _ = gc.Suite(&jemSuite{})

func (s *jemSuite) SetUpTest(c *gc.C) {
	s.JujuConnSuite.SetUpTest(c)
	s.sessionPool = mgosession.NewPool(context.TODO(), s.Session, 5)
	pool, err := jem.NewPool(context.TODO(), jem.Params{
		DB:              s.Session.DB("jem"),
		ControllerAdmin: "controller-admin",
		SessionPool:     s.sessionPool,
	})
	c.Assert(err, gc.IsNil)
	s.pool = pool
	s.jem = s.pool.JEM(context.TODO())
}

func (s *jemSuite) TearDownTest(c *gc.C) {
	s.jem.Close()
	s.pool.Close()
	s.sessionPool.Close()
	s.JujuConnSuite.TearDownTest(c)
}

func (s *jemSuite) TestPoolRequiresControllerAdmin(c *gc.C) {
	pool, err := jem.NewPool(context.TODO(), jem.Params{
		DB: s.Session.DB("jem"),
	})
	c.Assert(err, gc.ErrorMatches, "no controller admin group specified")
	c.Assert(pool, gc.IsNil)
}

func (s *jemSuite) TestPoolDoesNotReuseDeadConnection(c *gc.C) {
	session := jt.NewProxiedSession(c)
	defer session.Close()
	sessionPool := mgosession.NewPool(context.TODO(), session.Session, 3)
	defer sessionPool.Close()
	pool, err := jem.NewPool(context.TODO(), jem.Params{
		DB:              session.DB("jem"),
		ControllerAdmin: "controller-admin",
		SessionPool:     sessionPool,
	})
	c.Assert(err, gc.IsNil)
	defer pool.Close()

	assertOK := func(j *jem.JEM) {
		_, err := j.DB.Model(testContext, params.EntityPath{"bob", "x"})
		c.Assert(errgo.Cause(err), gc.Equals, params.ErrNotFound)
	}
	assertBroken := func(j *jem.JEM) {
		_, err := j.DB.Model(testContext, params.EntityPath{"bob", "x"})
		c.Assert(err, gc.ErrorMatches, `cannot get model "bob/x": EOF`)
	}

	// Get a JEM instance and perform a single operation so that the session used by the
	// JEM instance obtains a mongo socket.
	c.Logf("make jem0")
	jem0 := pool.JEM(context.TODO())
	defer jem0.Close()
	assertOK(jem0)

	c.Logf("close connections")
	// Close all current connections to the mongo instance,
	// which should cause subsequent operations on jem1 to fail.
	session.CloseConns()

	// Get another JEM instance, which should be a new session,
	// so operations on it should not fail.
	c.Logf("make jem1")
	jem1 := pool.JEM(context.TODO())
	defer jem1.Close()
	assertOK(jem1)

	// Get another JEM instance which should clone the same session
	// used by jem0 because only two sessions are available.
	c.Logf("make jem2")
	jem2 := pool.JEM(context.TODO())
	defer jem2.Close()

	// Perform another operation on jem0, which should fail and
	// cause its session not to be reused.
	c.Logf("check jem0 is broken")
	assertBroken(jem0)

	// The jem1 connection should still be working because it
	// was created after the connections were broken.
	c.Logf("check jem1 is ok")
	assertOK(jem1)

	c.Logf("check jem2 is ok")
	// The jem2 connection should also be broken because it
	// reused the same sessions as jem0
	assertBroken(jem2)

	// Get another instance, which should reuse the jem3 connection
	// and work OK.
	c.Logf("make jem3")
	jem3 := pool.JEM(context.TODO())
	defer jem3.Close()
	assertOK(jem3)

	// When getting the next instance, we should see that the connection
	// that we would have used is broken and create another one.
	c.Logf("make jem4")
	jem4 := pool.JEM(context.TODO())
	defer jem4.Close()
	assertOK(jem4)
}

func (s *jemSuite) TestClone(c *gc.C) {
	j := s.jem.Clone()
	j.Close()
	_, err := s.jem.DB.Model(testContext, params.EntityPath{"bob", "x"})
	c.Assert(errgo.Cause(err), gc.Equals, params.ErrNotFound)
}

var createModelTests = []struct {
	about            string
	user             string
	params           jem.CreateModelParams
	expectCredential params.CredentialPath
	expectError      string
	expectErrorCause error
}{{
	about: "success",
	user:  "bob",
	params: jem.CreateModelParams{
		Path: params.EntityPath{"bob", ""},
		Credential: params.CredentialPath{
			Cloud:      "dummy",
			EntityPath: params.EntityPath{"bob", "cred1"},
		},
		Cloud: "dummy",
	},
}, {
	about: "success specified controller",
	user:  "bob",
	params: jem.CreateModelParams{
		Path:           params.EntityPath{"bob", ""},
		ControllerPath: params.EntityPath{"bob", "controller"},
		Credential: params.CredentialPath{
			Cloud:      "dummy",
			EntityPath: params.EntityPath{"bob", "cred1"},
		},
		Cloud: "dummy",
	},
}, {
	about: "success with region",
	user:  "bob",
	params: jem.CreateModelParams{
		Path: params.EntityPath{"bob", ""},
		Credential: params.CredentialPath{
			Cloud:      "dummy",
			EntityPath: params.EntityPath{"bob", "cred1"},
		},
		Cloud:  "dummy",
		Region: "dummy-region",
	},
}, {
	about: "unknown credential",
	user:  "bob",
	params: jem.CreateModelParams{
		Path: params.EntityPath{"bob", ""},
		Credential: params.CredentialPath{
			Cloud:      "dummy",
			EntityPath: params.EntityPath{"bob", "cred2"},
		},
		Cloud: "dummy",
	},
	expectError:      `credential "dummy/bob/cred2" not found`,
	expectErrorCause: params.ErrNotFound,
}, {
	about: "model exists",
	user:  "bob",
	params: jem.CreateModelParams{
		Path: params.EntityPath{"bob", "oldmodel"},
		Credential: params.CredentialPath{
			Cloud:      "dummy",
			EntityPath: params.EntityPath{"bob", "cred1"},
		},
		Cloud: "dummy",
	},
	expectError:      `already exists`,
	expectErrorCause: params.ErrAlreadyExists,
}, {
	about: "unrecognised region",
	user:  "bob",
	params: jem.CreateModelParams{
		Path: params.EntityPath{"bob", ""},
		Credential: params.CredentialPath{
			Cloud:      "dummy",
			EntityPath: params.EntityPath{"bob", "cred1"},
		},
		Cloud:  "dummy",
		Region: "not-a-region",
	},
	expectError: `cannot select controller: no matching controllers found`,
}, {
	about: "empty cloud credentials selects single choice",
	user:  "bob",
	params: jem.CreateModelParams{
		Path:  params.EntityPath{"bob", ""},
		Cloud: "dummy",
	},
	expectCredential: params.CredentialPath{
		Cloud:      "dummy",
		EntityPath: params.EntityPath{"bob", "cred1"},
	},
}, {
	about: "empty cloud credentials fails with more than one choice",
	user:  "alice",
	params: jem.CreateModelParams{
		Path:  params.EntityPath{"alice", ""},
		Cloud: "dummy",
	},
	expectError:      `more than one possible credential to use`,
	expectErrorCause: params.ErrAmbiguousChoice,
}, {
	about: "empty cloud credentials passed through if no credentials found",
	user:  "charlie",
	params: jem.CreateModelParams{
		Path:  params.EntityPath{"charlie", ""},
		Cloud: "dummy",
	},
}}

func (s *jemSuite) TestCreateModel(c *gc.C) {
	now := bson.Now()
	s.PatchValue(jem.WallClock, jt.NewClock(now))
	ctlId := s.addController(c, params.EntityPath{"bob", "controller"})
	err := s.jem.DB.SetACL(testContext, s.jem.DB.Controllers(), ctlId, params.ACL{
		Read: []string{"everyone"},
	})
	c.Assert(err, gc.IsNil)
	// Bob has a single credential.
	err = jem.UpdateCredential(s.jem.DB, testContext, &mongodoc.Credential{
		Path: credentialPath("dummy", "bob", "cred1"),
		Type: "empty",
	})
	c.Assert(err, jc.ErrorIsNil)
	// Alice has two credentials.
	err = jem.UpdateCredential(s.jem.DB, testContext, &mongodoc.Credential{
		Path: credentialPath("dummy", "alice", "cred1"),
		Type: "empty",
	})
	c.Assert(err, jc.ErrorIsNil)
	err = jem.UpdateCredential(s.jem.DB, testContext, &mongodoc.Credential{
		Path: credentialPath("dummy", "alice", "cred2"),
		Type: "empty",
	})
	c.Assert(err, jc.ErrorIsNil)

	ctx := auth.ContextWithUser(testContext, "bob")
	// Create a model so that we can have a test case for an already-existing model
	_, err = s.jem.CreateModel(ctx, jem.CreateModelParams{
		Path:           params.EntityPath{"bob", "oldmodel"},
		ControllerPath: ctlId,
		Credential: params.CredentialPath{
			Cloud:      "dummy",
			EntityPath: params.EntityPath{"bob", "cred1"},
		},
		Cloud: "dummy",
	})
	c.Assert(err, jc.ErrorIsNil)
	for i, test := range createModelTests {
		c.Logf("test %d. %s", i, test.about)
		if test.params.Path.Name == "" {
			test.params.Path.Name = params.Name(fmt.Sprintf("test-%d", i))
		}
		m, err := s.jem.CreateModel(auth.ContextWithUser(testContext, test.user), test.params)
		if test.expectError != "" {
			c.Assert(err, gc.ErrorMatches, test.expectError)
			if test.expectErrorCause != nil {
				c.Assert(errgo.Cause(err), gc.Equals, test.expectErrorCause)
			}
			continue
		}
		c.Assert(err, jc.ErrorIsNil)
		c.Assert(m.Path, jc.DeepEquals, test.params.Path)
		c.Assert(m.UUID, gc.Not(gc.Equals), "")
		c.Assert(m.CreationTime.Equal(now), gc.Equals, true)
		c.Assert(m.Creator, gc.Equals, test.user)
		c.Assert(m.Cloud, gc.Equals, test.params.Cloud)
		c.Assert(m.CloudRegion, gc.Equals, "dummy-region")
		if !test.expectCredential.IsZero() {
			c.Assert(m.Credential, jc.DeepEquals, test.expectCredential)
		} else {
			c.Assert(m.Credential, jc.DeepEquals, test.params.Credential)
		}
		c.Assert(m.DefaultSeries, gc.Equals, "xenial")
		c.Assert(m.Life, gc.Equals, "alive")
	}
}

func (s *jemSuite) TestGrantModel(c *gc.C) {
	model := s.bootstrapModel(c, params.EntityPath{User: "bob", Name: "model"})
	conn, err := s.jem.OpenAPI(testContext, model.Controller)
	c.Assert(err, jc.ErrorIsNil)
	defer conn.Close()
	err = s.jem.GrantModel(testContext, conn, model, "alice", "write")
	c.Assert(err, jc.ErrorIsNil)
	model1, err := s.jem.DB.Model(testContext, model.Path)
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(model1.ACL, jc.DeepEquals, params.ACL{Read: []string{"alice"}})
}

func (s *jemSuite) TestGrantModelControllerFailure(c *gc.C) {
	model := s.bootstrapModel(c, params.EntityPath{User: "bob", Name: "model"})
	conn, err := s.jem.OpenAPI(testContext, model.Controller)
	c.Assert(err, jc.ErrorIsNil)
	defer conn.Close()
	err = s.jem.GrantModel(testContext, conn, model, "alice", "superpowers")
	c.Assert(err, gc.ErrorMatches, `"superpowers" model access not valid`)
	model1, err := s.jem.DB.Model(testContext, model.Path)
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(model1.ACL, jc.DeepEquals, params.ACL{Read: []string{}})
}

func (s *jemSuite) TestRevokeModel(c *gc.C) {
	model := s.bootstrapModel(c, params.EntityPath{User: "bob", Name: "model"})
	conn, err := s.jem.OpenAPI(testContext, model.Controller)
	c.Assert(err, jc.ErrorIsNil)
	defer conn.Close()
	err = s.jem.GrantModel(testContext, conn, model, "alice", "write")
	c.Assert(err, jc.ErrorIsNil)
	model1, err := s.jem.DB.Model(testContext, model.Path)
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(model1.ACL, jc.DeepEquals, params.ACL{Read: []string{"alice"}})
	err = s.jem.RevokeModel(testContext, conn, model, "alice", "write")
	c.Assert(err, jc.ErrorIsNil)
	model1, err = s.jem.DB.Model(testContext, model.Path)
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(model1.ACL, jc.DeepEquals, params.ACL{Read: []string{}})
}

func (s *jemSuite) TestRevokeModelControllerFailure(c *gc.C) {
	model := s.bootstrapModel(c, params.EntityPath{User: "bob", Name: "model"})
	conn, err := s.jem.OpenAPI(testContext, model.Controller)
	c.Assert(err, jc.ErrorIsNil)
	defer conn.Close()
	err = s.jem.GrantModel(testContext, conn, model, "alice", "write")
	c.Assert(err, jc.ErrorIsNil)
	model1, err := s.jem.DB.Model(testContext, model.Path)
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(model1.ACL, jc.DeepEquals, params.ACL{Read: []string{"alice"}})
	err = s.jem.RevokeModel(testContext, conn, model, "alice", "superpowers")
	c.Assert(err, gc.ErrorMatches, `"superpowers" model access not valid`)
	model1, err = s.jem.DB.Model(testContext, model.Path)
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(model1.ACL, jc.DeepEquals, params.ACL{Read: []string{}})
}

func (s *jemSuite) TestDestroyModel(c *gc.C) {
	model := s.bootstrapModel(c, params.EntityPath{User: "bob", Name: "model"})
	conn, err := s.jem.OpenAPI(testContext, model.Controller)
	c.Assert(err, jc.ErrorIsNil)
	defer conn.Close()

	// Sanity check the model exists
	client := modelmanagerapi.NewClient(conn)
	models, err := client.ListModels("bob@external")
	c.Assert(err, jc.ErrorIsNil)
	var found bool
	for _, m := range models {
		if m.UUID == model.UUID {
			c.Logf("found %#v", m)
			found = true
			break
		}
	}
	c.Assert(found, gc.Equals, true)

	ch := waitForDestruction(conn, c, model.UUID)

	err = s.jem.DestroyModel(testContext, conn, model)
	c.Assert(err, jc.ErrorIsNil)

	select {
	case <-ch:
	case <-time.After(jujujujutesting.LongWait):
		c.Fatalf("model not destroyed")
	}

	// Check the model is removed.
	_, err = s.jem.DB.Model(testContext, model.Path)
	c.Assert(errgo.Cause(err), gc.Equals, params.ErrNotFound)

	// Check that it cannot be destroyed twice
	err = s.jem.DestroyModel(testContext, conn, model)
	c.Assert(err, gc.ErrorMatches, `model "bob/model" not found`)

	// Put the model back in the database
	err = s.jem.DB.AddModel(testContext, model)
	c.Assert(err, jc.ErrorIsNil)

	// Check that it can still be removed even if the contoller has no model.
	err = s.jem.DestroyModel(testContext, conn, model)
	c.Assert(err, jc.ErrorIsNil)

	// Ensure the model is removed.
	_, err = s.jem.DB.Model(testContext, model.Path)
	c.Assert(errgo.Cause(err), gc.Equals, params.ErrNotFound)
}

func waitForDestruction(conn *apiconn.Conn, c *gc.C, uuid string) <-chan struct{} {
	ch := make(chan struct{})
	watcher, err := controller.NewClient(conn).WatchAllModels()
	go func() {
		defer close(ch)
		if !c.Check(err, jc.ErrorIsNil) {
			return
		}
		for {
			deltas, err := watcher.Next()
			if !c.Check(err, jc.ErrorIsNil) {
				return
			}
			for _, d := range deltas {
				d, ok := d.Entity.(*multiwatcher.ModelInfo)
				if ok && d.ModelUUID == uuid && d.Life == "dead" {
					return
				}
			}
		}
	}()
	return ch
}

func (s *jemSuite) TestUpdateCredential(c *gc.C) {
	ctlPath := s.addController(c, params.EntityPath{User: "bob", Name: "controller"})
	credPath := credentialPath("dummy", "bob", "cred")
	cred := &mongodoc.Credential{
		Path: credPath,
		Type: "empty",
	}
	err := jem.UpdateCredential(s.jem.DB, testContext, cred)
	c.Assert(err, jc.ErrorIsNil)
	conn, err := s.jem.OpenAPI(testContext, ctlPath)
	c.Assert(err, jc.ErrorIsNil)
	defer conn.Close()

	err = jem.UpdateControllerCredential(s.jem, testContext, ctlPath, cred.Path, conn, cred)
	c.Assert(err, jc.ErrorIsNil)
	err = jem.CredentialAddController(s.jem.DB, testContext, credPath, ctlPath)
	c.Assert(err, jc.ErrorIsNil)

	// Sanity check it was deployed
	client := cloudapi.NewClient(conn)
	credTag := names.NewCloudCredentialTag("dummy/bob@external/cred")
	creds, err := client.Credentials(credTag)
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(creds, jc.DeepEquals, []jujuparams.CloudCredentialResult{{
		Result: &jujuparams.CloudCredential{
			AuthType: "empty",
		},
	}})

	err = s.jem.UpdateCredential(testContext, &mongodoc.Credential{
		Path: credPath,
		Type: "userpass",
		Attributes: map[string]string{
			"username": "cloud-user",
			"password": "cloud-pass",
		},
	})
	c.Assert(err, jc.ErrorIsNil)

	// check it was updated on the controller.
	creds, err = client.Credentials(credTag)
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(creds, jc.DeepEquals, []jujuparams.CloudCredentialResult{{
		Result: &jujuparams.CloudCredential{
			AuthType: "userpass",
			Attributes: map[string]string{
				"username": "cloud-user",
			},
			Redacted: []string{
				"password",
			},
		},
	}})

	// Revoke the credential
	err = s.jem.UpdateCredential(testContext, &mongodoc.Credential{
		Path:    credPath,
		Revoked: true,
	})
	c.Assert(err, jc.ErrorIsNil)

	// check it was removed on the controller.
	creds, err = client.Credentials(credTag)
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(creds, jc.DeepEquals, []jujuparams.CloudCredentialResult{{
		Error: &jujuparams.Error{
			Code:    "not found",
			Message: `credential "cred" not found`,
		},
	}})
}

func (s *jemSuite) TestControllerUpdateCredentials(c *gc.C) {
	ctlPath := s.addController(c, params.EntityPath{User: "bob", Name: "controller"})
	credPath := credentialPath("dummy", "bob", "cred")
	credTag := names.NewCloudCredentialTag("dummy/bob@external/cred")
	cred := &mongodoc.Credential{
		Path: credPath,
		Type: "empty",
	}
	err := jem.UpdateCredential(s.jem.DB, testContext, cred)
	c.Assert(err, jc.ErrorIsNil)

	err = jem.SetCredentialUpdates(s.jem.DB, testContext, []params.EntityPath{ctlPath}, credPath)
	c.Assert(err, jc.ErrorIsNil)

	err = s.jem.ControllerUpdateCredentials(testContext, ctlPath)
	c.Assert(err, jc.ErrorIsNil)

	// check it was updated on the controller.
	conn, err := s.jem.OpenAPI(testContext, ctlPath)
	c.Assert(err, jc.ErrorIsNil)
	defer conn.Close()

	client := cloudapi.NewClient(conn)
	creds, err := client.Credentials(credTag)
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(creds, jc.DeepEquals, []jujuparams.CloudCredentialResult{{
		Result: &jujuparams.CloudCredential{
			AuthType:   "empty",
			Attributes: nil,
			Redacted:   nil,
		},
	}})
}

var doContollerTests = []struct {
	about             string
	cloud             params.Cloud
	region            string
	expectControllers []params.EntityPath
}{{
	about: "no parameters",
	expectControllers: []params.EntityPath{{
		User: "alice",
		Name: "aws-eu-west-1",
	}, {
		User: "alice",
		Name: "aws-us-east-1",
	}, {
		User: "bob",
		Name: "aws-eu-west-1",
	}, {
		User: "bob",
		Name: "aws-us-east-1",
	}, {
		User: "bob",
		Name: "gce-us-east-1",
	}},
}, {
	about: "aws",
	cloud: "aws",
	expectControllers: []params.EntityPath{{
		User: "alice",
		Name: "aws-eu-west-1",
	}, {
		User: "alice",
		Name: "aws-us-east-1",
	}, {
		User: "bob",
		Name: "aws-eu-west-1",
	}, {
		User: "bob",
		Name: "aws-us-east-1",
	}},
}, {
	about:  "aws-us-east-1",
	cloud:  "aws",
	region: "us-east-1",
	expectControllers: []params.EntityPath{{
		User: "alice",
		Name: "aws-us-east-1",
	}, {
		User: "bob",
		Name: "aws-us-east-1",
	}},
}, {
	about:             "aws-us-east-1",
	cloud:             "aws",
	region:            "us-east-2",
	expectControllers: []params.EntityPath{},
}}

func (s *jemSuite) TestDoControllers(c *gc.C) {
	testControllers := []mongodoc.Controller{{
		Path: params.EntityPath{
			User: params.User("bob"),
			Name: "aws-us-east-1",
		},
		Cloud: mongodoc.Cloud{
			Name: "aws",
			Regions: []mongodoc.Region{{
				Name: "us-east-1",
			}},
		},
		Location: map[string]string{
			"cloud":  "aws",
			"region": "us-east-1",
		},
		Public: true,
	}, {
		Path: params.EntityPath{
			User: params.User("bob"),
			Name: "aws-eu-west-1",
		},
		Cloud: mongodoc.Cloud{
			Name: "aws",
			Regions: []mongodoc.Region{{
				Name: "eu-west-1",
			}},
		},
		Location: map[string]string{
			"cloud":  "aws",
			"region": "us-west-1",
		},
		Public: true,
	}, {
		Path: params.EntityPath{
			User: params.User("alice"),
			Name: "aws-us-east-1",
		},
		ACL: params.ACL{
			Read: []string{"bob-group"},
		},
		Cloud: mongodoc.Cloud{
			Name: "aws",
			Regions: []mongodoc.Region{{
				Name: "us-east-1",
			}},
		},
		Location: map[string]string{
			"cloud":  "aws",
			"region": "us-east-1",
		},
		Public: true,
	}, {
		Path: params.EntityPath{
			User: params.User("alice"),
			Name: "aws-eu-west-1",
		},
		ACL: params.ACL{
			Read: []string{"bob"},
		},
		Cloud: mongodoc.Cloud{
			Name: "aws",
			Regions: []mongodoc.Region{{
				Name: "eu-west-1",
			}},
		},
		Location: map[string]string{
			"cloud":  "aws",
			"region": "eu-west-1",
		},
		Public: true,
	}, {
		Path: params.EntityPath{
			User: params.User("alice"),
			Name: "aws-us-east-2",
		},
		Cloud: mongodoc.Cloud{
			Name: "aws",
			Regions: []mongodoc.Region{{
				Name: "us-east-1",
			}},
		},
		Location: map[string]string{
			"cloud":  "aws",
			"region": "us-east-1",
		},
		Public: true,
	}, {
		Path: params.EntityPath{
			User: params.User("bob"),
			Name: "gce-us-east-1",
		},
		Cloud: mongodoc.Cloud{
			Name: "gce",
			Regions: []mongodoc.Region{{
				Name: "us-east-1",
			}},
		},
		Location: map[string]string{
			"cloud":  "gce",
			"region": "us-east-1",
		},
		Public: true,
	}, {
		Path: params.EntityPath{
			User: params.User("alice"),
			Name: "gce-us-east-1",
		},
		Cloud: mongodoc.Cloud{
			Name: "gce",
			Regions: []mongodoc.Region{{
				Name: "us-east-1",
			}},
		},
		Location: map[string]string{
			"cloud":  "gce",
			"region": "us-east-1",
		},
		Public: true,
	}}
	for i := range testControllers {
		err := s.jem.DB.AddController(testContext, &testControllers[i])

		c.Assert(err, gc.IsNil)
	}
	ctx := auth.ContextWithUser(testContext, "bob", "bob-group")
	for i, test := range doContollerTests {
		c.Logf("test %d. %s", i, test.about)
		var obtainedControllers []params.EntityPath
		err := s.jem.DoControllers(ctx, test.cloud, test.region, func(ctl *mongodoc.Controller) error {
			obtainedControllers = append(obtainedControllers, ctl.Path)
			return nil
		})
		c.Assert(err, gc.IsNil)
		c.Assert(obtainedControllers, jc.DeepEquals, test.expectControllers)
	}
}

func (s *jemSuite) TestDoControllersErrorResponse(c *gc.C) {
	testControllers := []mongodoc.Controller{{
		Path: params.EntityPath{
			User: params.User("bob"),
			Name: "aws-us-east-1",
		},
		Cloud: mongodoc.Cloud{
			Name: "aws",
			Regions: []mongodoc.Region{{
				Name: "us-east-1",
			}},
		},
		Public: true,
	}, {
		Path: params.EntityPath{
			User: params.User("bob"),
			Name: "aws-eu-west-1",
		},
		Cloud: mongodoc.Cloud{
			Name: "aws",
			Regions: []mongodoc.Region{{
				Name: "eu-west-1",
			}},
		},
		Public: true,
	}, {
		Path: params.EntityPath{
			User: params.User("alice"),
			Name: "aws-us-east-1",
		},
		ACL: params.ACL{
			Read: []string{"bob-group"},
		},
		Cloud: mongodoc.Cloud{
			Name: "aws",
			Regions: []mongodoc.Region{{
				Name: "us-east-1",
			}},
		},
		Public: true,
	}, {
		Path: params.EntityPath{
			User: params.User("alice"),
			Name: "aws-eu-west-1",
		},
		ACL: params.ACL{
			Read: []string{"bob"},
		},
		Cloud: mongodoc.Cloud{
			Name: "aws",
			Regions: []mongodoc.Region{{
				Name: "eu-west-1",
			}},
		},
		Public: true,
	}, {
		Path: params.EntityPath{
			User: params.User("alice"),
			Name: "aws-us-east-2",
		},
		Cloud: mongodoc.Cloud{
			Name: "aws",
			Regions: []mongodoc.Region{{
				Name: "us-east-1",
			}},
		},
		Public: true,
	}, {
		Path: params.EntityPath{
			User: params.User("bob"),
			Name: "gce-us-east-1",
		},
		Cloud: mongodoc.Cloud{
			Name: "gce",
			Regions: []mongodoc.Region{{
				Name: "us-east-1",
			}},
		},
		Public: true,
	}, {
		Path: params.EntityPath{
			User: params.User("alice"),
			Name: "gce-us-east-1",
		},
		Cloud: mongodoc.Cloud{
			Name: "gce",
			Regions: []mongodoc.Region{{
				Name: "us-east-1",
			}},
		},
		Public: true,
	}}
	for i := range testControllers {
		err := s.jem.DB.AddController(testContext, &testControllers[i])

		c.Assert(err, gc.IsNil)
	}
	ctx := auth.ContextWithUser(testContext, "bob", "bob-group")
	testCause := errgo.New("test-cause")
	err := s.jem.DoControllers(ctx, "", "", func(ctl *mongodoc.Controller) error {
		return errgo.WithCausef(nil, testCause, "test error")
	})
	c.Assert(errgo.Cause(err), gc.Equals, testCause)
}

var selectContollerTests = []struct {
	about            string
	cloud            params.Cloud
	region           string
	randIntn         func(int) int
	expectController params.EntityPath
	expectError      string
	expectErrorCause error
}{{
	about: "no parameters",
	randIntn: func(n int) int {
		return 4
	},
	expectController: params.EntityPath{
		User: "bob",
		Name: "gce-us-east-1",
	},
}, {
	about: "aws",
	cloud: "aws",
	randIntn: func(n int) int {
		return 1
	},
	expectController: params.EntityPath{
		User: "alice",
		Name: "aws-us-east-1",
	},
}, {
	about:  "aws-us-east-1",
	cloud:  "aws",
	region: "us-east-1",
	randIntn: func(n int) int {
		return 1
	},
	expectController: params.EntityPath{
		User: "bob",
		Name: "aws-us-east-1",
	},
}, {
	about:  "no match",
	cloud:  "aws",
	region: "us-east-2",
	randIntn: func(n int) int {
		return 1
	},
	expectError:      `no matching controllers found`,
	expectErrorCause: params.ErrNotFound,
}}

func (s *jemSuite) TestSelectController(c *gc.C) {
	var randIntn *func(int) int
	s.PatchValue(jem.RandIntn, func(n int) int {
		return (*randIntn)(n)
	})
	testControllers := []mongodoc.Controller{{
		Path: params.EntityPath{
			User: params.User("bob"),
			Name: "aws-us-east-1",
		},
		Cloud: mongodoc.Cloud{
			Name: "aws",
			Regions: []mongodoc.Region{{
				Name: "us-east-1",
			}},
		},
		Location: map[string]string{
			"cloud":  "aws",
			"region": "us-east-1",
		},
		Public: true,
	}, {
		Path: params.EntityPath{
			User: params.User("bob"),
			Name: "aws-eu-west-1",
		},
		Cloud: mongodoc.Cloud{
			Name: "aws",
			Regions: []mongodoc.Region{{
				Name: "eu-west-1",
			}},
		},
		Location: map[string]string{
			"cloud":  "aws",
			"region": "eu-west-1",
		},
		Public: true,
	}, {
		Path: params.EntityPath{
			User: params.User("alice"),
			Name: "aws-us-east-1",
		},
		ACL: params.ACL{
			Read: []string{"bob-group"},
		},
		Cloud: mongodoc.Cloud{
			Name: "aws",
			Regions: []mongodoc.Region{{
				Name: "us-east-1",
			}},
		},
		Location: map[string]string{
			"cloud":  "aws",
			"region": "us-east-1",
		},
		Public: true,
	}, {
		Path: params.EntityPath{
			User: params.User("alice"),
			Name: "aws-eu-west-1",
		},
		ACL: params.ACL{
			Read: []string{"bob"},
		},
		Cloud: mongodoc.Cloud{
			Name: "aws",
			Regions: []mongodoc.Region{{
				Name: "eu-west-1",
			}},
		},
		Location: map[string]string{
			"cloud":  "aws",
			"region": "eu-west-1",
		},
		Public: true,
	}, {
		Path: params.EntityPath{
			User: params.User("alice"),
			Name: "aws-us-east-2",
		},
		Cloud: mongodoc.Cloud{
			Name: "aws",
			Regions: []mongodoc.Region{{
				Name: "us-east-1",
			}},
		},
		Location: map[string]string{
			"cloud":  "aws",
			"region": "us-east-1",
		},
		Public: true,
	}, {
		Path: params.EntityPath{
			User: params.User("bob"),
			Name: "gce-us-east-1",
		},
		Cloud: mongodoc.Cloud{
			Name: "gce",
			Regions: []mongodoc.Region{{
				Name: "us-east-1",
			}},
		},
		Location: map[string]string{
			"cloud":  "gce",
			"region": "us-east-1",
		},
		Public: true,
	}, {
		Path: params.EntityPath{
			User: params.User("alice"),
			Name: "gce-us-east-1",
		},
		Cloud: mongodoc.Cloud{
			Name: "gce",
			Regions: []mongodoc.Region{{
				Name: "us-east-1",
			}},
		},
		Location: map[string]string{
			"cloud":  "gce",
			"region": "us-east-1",
		},
		Public: true,
	}}
	for i := range testControllers {
		err := s.jem.DB.AddController(testContext, &testControllers[i])

		c.Assert(err, gc.IsNil)
	}
	ctx := auth.ContextWithUser(testContext, "bob", "bob-group")
	for i, test := range selectContollerTests {
		c.Logf("test %d. %s", i, test.about)
		randIntn = &test.randIntn
		ctl, err := jem.SelectController(s.jem, ctx, test.cloud, test.region)
		if test.expectError != "" {
			c.Assert(err, gc.ErrorMatches, test.expectError)
			if test.expectErrorCause != nil {
				c.Assert(errgo.Cause(err), gc.Equals, test.expectErrorCause)
			}
			continue
		}
		c.Assert(err, gc.IsNil)
		c.Assert(ctl, jc.DeepEquals, test.expectController)
	}
}

var controllerTests = []struct {
	path             params.EntityPath
	expectErrorCause error
}{{
	path: params.EntityPath{"bob", "controller"},
}, {
	path: params.EntityPath{"bob-group", "controller"},
}, {
	path:             params.EntityPath{"alice", "controller"},
	expectErrorCause: params.ErrUnauthorized,
}, {
	path:             params.EntityPath{"bob", "controller2"},
	expectErrorCause: params.ErrNotFound,
}, {
	path:             params.EntityPath{"bob-group", "controller2"},
	expectErrorCause: params.ErrNotFound,
}, {
	path:             params.EntityPath{"alice", "controller2"},
	expectErrorCause: params.ErrUnauthorized,
}}

func (s *jemSuite) TestController(c *gc.C) {
	s.addController(c, params.EntityPath{"alice", "controller"})
	s.addController(c, params.EntityPath{"bob", "controller"})
	s.addController(c, params.EntityPath{"bob-group", "controller"})
	ctx := auth.ContextWithUser(testContext, "bob", "bob-group")

	for i, test := range controllerTests {
		c.Logf("tes %d. %s", i, test.path)
		ctl, err := s.jem.Controller(ctx, test.path)
		if test.expectErrorCause != nil {
			c.Assert(errgo.Cause(err), gc.Equals, test.expectErrorCause)
			continue
		}
		c.Assert(err, jc.ErrorIsNil)
		c.Assert(ctl.Path, jc.DeepEquals, test.path)
	}
}

var credentialTests = []struct {
	path             params.CredentialPath
	expectErrorCause error
}{{
	path: params.CredentialPath{
		Cloud:      "dummy",
		EntityPath: params.EntityPath{"bob", "credential"},
	},
}, {
	path: params.CredentialPath{
		Cloud:      "dummy",
		EntityPath: params.EntityPath{"bob-group", "credential"},
	},
}, {
	path: params.CredentialPath{
		Cloud:      "dummy",
		EntityPath: params.EntityPath{"alice", "credential"},
	},
	expectErrorCause: params.ErrUnauthorized,
}, {
	path: params.CredentialPath{
		Cloud:      "dummy",
		EntityPath: params.EntityPath{"bob", "credential2"},
	},
	expectErrorCause: params.ErrNotFound,
}, {
	path: params.CredentialPath{
		Cloud:      "dummy",
		EntityPath: params.EntityPath{"bob-group", "credential2"},
	},
	expectErrorCause: params.ErrNotFound,
}, {
	path: params.CredentialPath{
		Cloud:      "dummy",
		EntityPath: params.EntityPath{"alice", "credential2"},
	},
	expectErrorCause: params.ErrUnauthorized,
}}

func (s *jemSuite) TestCredential(c *gc.C) {
	creds := []mongodoc.Credential{{
		Path: params.CredentialPath{
			Cloud:      "dummy",
			EntityPath: params.EntityPath{"alice", "credential"},
		},
	}, {
		Path: params.CredentialPath{
			Cloud:      "dummy",
			EntityPath: params.EntityPath{"bob", "credential"},
		},
	}, {
		Path: params.CredentialPath{
			Cloud:      "dummy",
			EntityPath: params.EntityPath{"bob-group", "credential"},
		},
	}}
	for _, cred := range creds {
		cred.Id = cred.Path.String()
		jem.UpdateCredential(s.jem.DB, testContext, &cred)
	}
	ctx := auth.ContextWithUser(testContext, "bob", "bob-group")

	for i, test := range credentialTests {
		c.Logf("tes %d. %s", i, test.path)
		ctl, err := s.jem.Credential(ctx, test.path)
		if test.expectErrorCause != nil {
			c.Assert(errgo.Cause(err), gc.Equals, test.expectErrorCause)
			continue
		}
		c.Assert(err, jc.ErrorIsNil)
		c.Assert(ctl.Path, jc.DeepEquals, test.path)
	}
}

func (s *jemSuite) TestUserTag(c *gc.C) {
	c.Assert(jem.UserTag(params.User("alice")).String(), gc.Equals, "user-alice@external")
	c.Assert(jem.UserTag(params.User("alice@domain")).String(), gc.Equals, "user-alice@domain")
}

func (s *jemSuite) TestCloudCredentialTag(c *gc.C) {
	cp1 := params.CredentialPath{
		Cloud: "dummy",
		EntityPath: params.EntityPath{
			User: "alice",
			Name: "cred",
		},
	}
	cp2 := params.CredentialPath{
		Cloud: "dummy",
		EntityPath: params.EntityPath{
			User: "alice@domain",
			Name: "cred",
		},
	}
	c.Assert(jem.CloudCredentialTag(cp1).String(), gc.Equals, "cloudcred-dummy_alice@external_cred")
	c.Assert(jem.CloudCredentialTag(cp2).String(), gc.Equals, "cloudcred-dummy_alice@domain_cred")
}

func (s *jemSuite) addController(c *gc.C, path params.EntityPath) params.EntityPath {
	info := s.APIInfo(c)

	hps, err := mongodoc.ParseAddresses(info.Addrs)
	c.Assert(err, jc.ErrorIsNil)

	ctl := &mongodoc.Controller{
		Path:          path,
		HostPorts:     [][]mongodoc.HostPort{hps},
		CACert:        info.CACert,
		AdminUser:     info.Tag.Id(),
		AdminPassword: info.Password,
		Cloud: mongodoc.Cloud{
			Name: "dummy",
			Regions: []mongodoc.Region{{
				Name: "dummy-region",
			}},
		},
		Location: map[string]string{
			"cloud":  "dummy",
			"region": "dummy-region",
		},
		Public: true,
	}
	err = s.jem.DB.AddController(testContext, ctl)
	c.Assert(err, jc.ErrorIsNil)
	return path
}

func (s *jemSuite) bootstrapModel(c *gc.C, path params.EntityPath) *mongodoc.Model {
	ctlPath := s.addController(c, params.EntityPath{User: path.User, Name: "controller"})
	credPath := credentialPath("dummy", string(path.User), "cred")
	err := jem.UpdateCredential(s.jem.DB, testContext, &mongodoc.Credential{
		Path: credPath,
		Type: "empty",
	})
	c.Assert(err, jc.ErrorIsNil)
	ctx := auth.ContextWithUser(testContext, string(path.User))
	model, err := s.jem.CreateModel(ctx, jem.CreateModelParams{
		Path:           path,
		ControllerPath: ctlPath,
		Credential: params.CredentialPath{
			Cloud:      "dummy",
			EntityPath: params.EntityPath{"bob", "cred"},
		},
		Cloud: "dummy",
	})
	c.Assert(err, jc.ErrorIsNil)
	return model
}

// fakeUUID returns something that looks like a UUID but actually uses n.
func fakeUUID(n int) string {
	return fmt.Sprintf("00000000-0000-0000-0000-00000000%.04x", n)
}
