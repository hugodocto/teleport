/*
 * Teleport
 * Copyright (C) 2024  Gravitational, Inc.
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Affero General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU Affero General Public License for more details.
 *
 * You should have received a copy of the GNU Affero General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 */

package usertasksv1

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/gravitational/trace"
	"github.com/stretchr/testify/require"

	usertasksv1 "github.com/gravitational/teleport/api/gen/proto/go/teleport/usertasks/v1"
	"github.com/gravitational/teleport/api/types"
	"github.com/gravitational/teleport/api/types/usertasks"
	"github.com/gravitational/teleport/lib/authz"
	"github.com/gravitational/teleport/lib/backend/memory"
	"github.com/gravitational/teleport/lib/services"
	"github.com/gravitational/teleport/lib/services/local"
	usagereporter "github.com/gravitational/teleport/lib/usagereporter/teleport"
	"github.com/gravitational/teleport/lib/utils"
)

func TestServiceAccess(t *testing.T) {
	t.Parallel()
	testReporter := &mockUsageReporter{}

	testCases := []struct {
		name          string
		allowedVerbs  []string
		allowedStates []authz.AdminActionAuthState
	}{
		{
			name:         "CreateUserTask",
			allowedVerbs: []string{types.VerbCreate},
		},
		{
			name:         "UpdateUserTask",
			allowedVerbs: []string{types.VerbUpdate},
		},
		{
			name:         "DeleteUserTask",
			allowedVerbs: []string{types.VerbDelete},
		},
		{
			name:         "UpsertUserTask",
			allowedVerbs: []string{types.VerbCreate, types.VerbUpdate},
		},
		{
			name:         "ListUserTasks",
			allowedVerbs: []string{types.VerbRead, types.VerbList},
		},
		{
			name:         "ListUserTasksByIntegration",
			allowedVerbs: []string{types.VerbRead, types.VerbList},
		},
		{
			name:         "GetUserTask",
			allowedVerbs: []string{types.VerbRead},
		},
	}

	for _, tt := range testCases {
		t.Run(tt.name, func(t *testing.T) {
			for _, verbs := range utils.Combinations(tt.allowedVerbs) {
				t.Run(fmt.Sprintf("verbs=%v", verbs), func(t *testing.T) {
					service := newService(t, fakeChecker{allowedVerbs: verbs}, testReporter)
					err := callMethod(t, service, tt.name)
					// expect access denied except with full set of verbs.
					if len(verbs) == len(tt.allowedVerbs) {
						require.False(t, trace.IsAccessDenied(err))
					} else {
						require.True(t, trace.IsAccessDenied(err), "expected access denied for verbs %v, got err=%v", verbs, err)
					}
				})
			}
		})
	}

	// verify that all declared methods have matching test cases
	t.Run("verify coverage", func(t *testing.T) {
		for _, method := range usertasksv1.UserTaskService_ServiceDesc.Methods {
			t.Run(method.MethodName, func(t *testing.T) {
				match := false
				for _, testCase := range testCases {
					match = match || testCase.name == method.MethodName
				}
				require.True(t, match, "method %v without coverage, no matching tests", method.MethodName)
			})
		}
	})
}

func TestUsageEvents(t *testing.T) {
	rwVerbs := []string{types.VerbList, types.VerbCreate, types.VerbRead, types.VerbUpdate, types.VerbDelete}
	testReporter := &mockUsageReporter{}
	service := newService(t, fakeChecker{allowedVerbs: rwVerbs}, testReporter)
	ctx := context.Background()

	ut1, err := usertasks.NewDiscoverEC2UserTask(&usertasksv1.UserTaskSpec{
		Integration: "my-integration",
		TaskType:    "discover-ec2",
		IssueType:   "ec2-ssm-invocation-failure",
		State:       "OPEN",
		DiscoverEc2: &usertasksv1.DiscoverEC2{
			AccountId: "123456789012",
			Region:    "us-east-1",
			Instances: map[string]*usertasksv1.DiscoverEC2Instance{
				"i-123": &usertasksv1.DiscoverEC2Instance{
					InstanceId:      "i-123",
					DiscoveryConfig: "dc01",
					DiscoveryGroup:  "dg01",
				},
			},
		},
	})
	require.NoError(t, err)

	_, err = service.CreateUserTask(ctx, &usertasksv1.CreateUserTaskRequest{UserTask: ut1})
	require.NoError(t, err)
	// Usage reporting happens when user task is created, so we expect to see an event.
	require.Len(t, testReporter.emittedEvents, 1)

	ut1.Spec.DiscoverEc2.Instances["i-345"] = &usertasksv1.DiscoverEC2Instance{
		InstanceId:      "i-345",
		DiscoveryConfig: "dc01",
		DiscoveryGroup:  "dg01",
	}
	_, err = service.UpsertUserTask(ctx, &usertasksv1.UpsertUserTaskRequest{UserTask: ut1})
	require.NoError(t, err)
	// State was not updated, so usage events must not increase.
	require.Len(t, testReporter.emittedEvents, 1)

	ut1.Spec.State = "RESOLVED"
	_, err = service.UpdateUserTask(ctx, &usertasksv1.UpdateUserTaskRequest{UserTask: ut1})
	require.NoError(t, err)
	// State was updated, so usage events include this new usage report.
	require.Len(t, testReporter.emittedEvents, 2)
}

// callMethod calls a method with given name in the UserTask service
func callMethod(t *testing.T, service *Service, method string) error {
	for _, desc := range usertasksv1.UserTaskService_ServiceDesc.Methods {
		if desc.MethodName == method {
			_, err := desc.Handler(service, context.Background(), func(_ any) error { return nil }, nil)
			return err
		}
	}
	require.FailNow(t, "method %v not found", method)
	panic("this line should never be reached: FailNow() should interrupt the test")
}

type fakeChecker struct {
	allowedVerbs []string
	services.AccessChecker
}

func (f fakeChecker) CheckAccessToRule(_ services.RuleContext, _ string, resource string, verb string) error {
	if resource == types.KindUserTask {
		if slices.Contains(f.allowedVerbs, verb) {
			return nil
		}
	}

	return trace.AccessDenied("access denied to rule=%v/verb=%v", resource, verb)
}

func newService(t *testing.T, checker services.AccessChecker, usageReporter usagereporter.UsageReporter) *Service {
	t.Helper()

	b, err := memory.New(memory.Config{})
	require.NoError(t, err)

	backendService, err := local.NewUserTasksService(b)
	require.NoError(t, err)

	authorizer := authz.AuthorizerFunc(func(ctx context.Context) (*authz.Context, error) {
		user, err := types.NewUser("llama")
		if err != nil {
			return nil, err
		}
		return &authz.Context{
			User:    user,
			Checker: checker,
		}, nil
	})

	service, err := NewService(ServiceConfig{
		Authorizer:    authorizer,
		Backend:       backendService,
		Cache:         backendService,
		UsageReporter: func() usagereporter.UsageReporter { return usageReporter },
	})
	require.NoError(t, err)
	return service
}

type mockUsageReporter struct {
	emittedEvents []*usagereporter.UserTaskStateEvent
}

func (m *mockUsageReporter) AnonymizeAndSubmit(events ...usagereporter.Anonymizable) {
	for _, e := range events {
		if userTaskEvent, ok := e.(*usagereporter.UserTaskStateEvent); ok {
			m.emittedEvents = append(m.emittedEvents, userTaskEvent)
		}
	}
}
