package runtimetypes_test

import (
	"fmt"
	"testing"
	"time"

	libdb "github.com/contenox/beam/internal/libdbexec"
	"github.com/contenox/beam/internal/store/runtimetypes"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestUnit_groups_CreateAndGetgroup(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	group := &runtimetypes.AffinityGroup{
		ID:          uuid.NewString(),
		Name:        "TestUnit_GroupAffinity",
		PurposeType: "inference",
	}

	err := s.CreateAffinityGroup(ctx, group)
	require.NoError(t, err)
	require.NotEmpty(t, group.ID)

	got, err := s.GetAffinityGroup(ctx, group.ID)
	require.NoError(t, err)
	require.Equal(t, group.Name, got.Name)
	require.Equal(t, group.PurposeType, got.PurposeType)
	require.WithinDuration(t, group.CreatedAt, got.CreatedAt, time.Second)
	require.WithinDuration(t, group.UpdatedAt, got.UpdatedAt, time.Second)
}

func TestUnit_groups_Updategroup(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	group := &runtimetypes.AffinityGroup{
		ID:          uuid.NewString(),
		Name:        "Initialgroup",
		PurposeType: "testing",
	}

	err := s.CreateAffinityGroup(ctx, group)
	require.NoError(t, err)

	group.Name = "Updatedgroup"
	group.PurposeType = "production"

	err = s.UpdateAffinityGroup(ctx, group)
	require.NoError(t, err)

	got, err := s.GetAffinityGroup(ctx, group.ID)
	require.NoError(t, err)
	require.Equal(t, "Updatedgroup", got.Name)
	require.Equal(t, "production", got.PurposeType)
	require.True(t, got.UpdatedAt.After(got.CreatedAt))
}

func TestUnit_groups_Deletegroup(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	group := &runtimetypes.AffinityGroup{
		ID:          uuid.NewString(),
		Name:        "ToDelete",
		PurposeType: "testing",
	}

	err := s.CreateAffinityGroup(ctx, group)
	require.NoError(t, err)

	err = s.DeleteAffinityGroup(ctx, group.ID)
	require.NoError(t, err)

	_, err = s.GetAffinityGroup(ctx, group.ID)
	require.ErrorIs(t, err, libdb.ErrNotFound)
}

func TestUnit_groups_Listgroups(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	groups, err := s.ListAffinityGroups(ctx, nil, 100)
	require.NoError(t, err)
	require.Empty(t, groups)

	group1 := &runtimetypes.AffinityGroup{ID: uuid.NewString(), Name: "group1", PurposeType: "type1"}
	group2 := &runtimetypes.AffinityGroup{ID: uuid.NewString(), Name: "group2", PurposeType: "type2"}

	err = s.CreateAffinityGroup(ctx, group1)
	require.NoError(t, err)
	err = s.CreateAffinityGroup(ctx, group2)
	require.NoError(t, err)

	groups, err = s.ListAffinityGroups(ctx, nil, 100)
	require.NoError(t, err)
	require.Len(t, groups, 2)
	require.Equal(t, group2.ID, groups[0].ID)
	require.Equal(t, group1.ID, groups[1].ID)
}

func TestUnit_groups_ListgroupsPagination(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	var createdgroups []*runtimetypes.AffinityGroup
	for i := range 5 {
		group := &runtimetypes.AffinityGroup{
			ID:          uuid.NewString(),
			Name:        fmt.Sprintf("pagination-group-%d", i),
			PurposeType: "inference",
		}
		err := s.CreateAffinityGroup(ctx, group)
		require.NoError(t, err)
		createdgroups = append(createdgroups, group)
	}

	var receivedgroups []*runtimetypes.AffinityGroup
	var lastCursor *time.Time
	limit := 2

	page1, err := s.ListAffinityGroups(ctx, lastCursor, limit)
	require.NoError(t, err)
	require.Len(t, page1, 2)
	receivedgroups = append(receivedgroups, page1...)
	lastCursor = &page1[len(page1)-1].CreatedAt

	page2, err := s.ListAffinityGroups(ctx, lastCursor, limit)
	require.NoError(t, err)
	require.Len(t, page2, 2)
	receivedgroups = append(receivedgroups, page2...)
	lastCursor = &page2[len(page2)-1].CreatedAt

	page3, err := s.ListAffinityGroups(ctx, lastCursor, limit)
	require.NoError(t, err)
	require.Len(t, page3, 1)
	receivedgroups = append(receivedgroups, page3...)

	page4, err := s.ListAffinityGroups(ctx, &page3[0].CreatedAt, limit)
	require.NoError(t, err)
	require.Empty(t, page4)

	require.Len(t, receivedgroups, 5)

	require.Equal(t, createdgroups[4].ID, receivedgroups[0].ID)
	require.Equal(t, createdgroups[3].ID, receivedgroups[1].ID)
	require.Equal(t, createdgroups[2].ID, receivedgroups[2].ID)
	require.Equal(t, createdgroups[1].ID, receivedgroups[3].ID)
	require.Equal(t, createdgroups[0].ID, receivedgroups[4].ID)
}

func TestUnit_groups_GetgroupByName(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	group := &runtimetypes.AffinityGroup{
		ID:          uuid.NewString(),
		Name:        "Uniquegroup",
		PurposeType: "inference",
	}

	err := s.CreateAffinityGroup(ctx, group)
	require.NoError(t, err)

	got, err := s.GetAffinityGroupByName(ctx, "Uniquegroup")
	require.NoError(t, err)
	require.Equal(t, group.ID, got.ID)
}

func TestUnit_groups_ListgroupsByPurpose(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	purpose := "inference"
	group1 := &runtimetypes.AffinityGroup{ID: uuid.NewString(), Name: "group1", PurposeType: purpose}
	group2 := &runtimetypes.AffinityGroup{ID: uuid.NewString(), Name: "group2", PurposeType: "training"}

	err := s.CreateAffinityGroup(ctx, group1)
	require.NoError(t, err)
	err = s.CreateAffinityGroup(ctx, group2)
	require.NoError(t, err)

	groups, err := s.ListAffinityGroupByPurpose(ctx, purpose, nil, 100)
	require.NoError(t, err)
	require.Len(t, groups, 1)
	require.Equal(t, group1.ID, groups[0].ID)
}

func TestUnit_groups_ListgroupsByPurposePagination(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	purpose := "inference"
	otherPurpose := "training"
	var createdgroups []*runtimetypes.AffinityGroup
	for i := range 5 {
		group := &runtimetypes.AffinityGroup{
			ID:          uuid.NewString(),
			Name:        fmt.Sprintf("inference-group-%d", i),
			PurposeType: purpose,
		}
		err := s.CreateAffinityGroup(ctx, group)
		require.NoError(t, err)
		createdgroups = append(createdgroups, group)
	}

	othergroup := &runtimetypes.AffinityGroup{
		ID:          uuid.NewString(),
		Name:        "other-group",
		PurposeType: otherPurpose,
	}
	err := s.CreateAffinityGroup(ctx, othergroup)
	require.NoError(t, err)

	var receivedgroups []*runtimetypes.AffinityGroup
	var lastCursor *time.Time
	limit := 2

	page1, err := s.ListAffinityGroupByPurpose(ctx, purpose, lastCursor, limit)
	require.NoError(t, err)
	require.Len(t, page1, 2)
	receivedgroups = append(receivedgroups, page1...)
	lastCursor = &page1[len(page1)-1].CreatedAt

	page2, err := s.ListAffinityGroupByPurpose(ctx, purpose, lastCursor, limit)
	require.NoError(t, err)
	require.Len(t, page2, 2)
	receivedgroups = append(receivedgroups, page2...)
	lastCursor = &page2[len(page2)-1].CreatedAt

	page3, err := s.ListAffinityGroupByPurpose(ctx, purpose, lastCursor, limit)
	require.NoError(t, err)
	require.Len(t, page3, 1)
	receivedgroups = append(receivedgroups, page3...)

	page4, err := s.ListAffinityGroupByPurpose(ctx, purpose, &page3[0].CreatedAt, limit)
	require.NoError(t, err)
	require.Empty(t, page4)

	require.Len(t, receivedgroups, 5)
	require.Equal(t, createdgroups[4].ID, receivedgroups[0].ID)
	require.Equal(t, createdgroups[3].ID, receivedgroups[1].ID)
	require.Equal(t, createdgroups[2].ID, receivedgroups[2].ID)
	require.Equal(t, createdgroups[1].ID, receivedgroups[3].ID)
	require.Equal(t, createdgroups[0].ID, receivedgroups[4].ID)

	for _, p := range receivedgroups {
		require.Equal(t, purpose, p.PurposeType)
	}
}

func TestUnit_groups_AssignAndListBackendsForgroup(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	group := &runtimetypes.AffinityGroup{ID: uuid.NewString(), Name: "group1"}
	err := s.CreateAffinityGroup(ctx, group)
	require.NoError(t, err)

	backend := &runtimetypes.Backend{
		ID:      uuid.NewString(),
		Name:    "Backend1",
		BaseURL: "http://backend1",
		Type:    "ollama",
	}
	err = s.CreateBackend(ctx, backend)
	require.NoError(t, err)

	err = s.AssignBackendToAffinityGroup(ctx, group.ID, backend.ID)
	require.NoError(t, err)

	backends, err := s.ListBackendsForAffinityGroup(ctx, group.ID)
	require.NoError(t, err)
	require.Len(t, backends, 1)
	require.Equal(t, backend.ID, backends[0].ID)
}

func TestUnit_groups_RemoveBackendFromgroup(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	group := &runtimetypes.AffinityGroup{ID: uuid.NewString(), Name: "group1"}
	err := s.CreateAffinityGroup(ctx, group)
	require.NoError(t, err)

	backend := &runtimetypes.Backend{ID: uuid.NewString(), Name: "Backend1"}
	err = s.CreateBackend(ctx, backend)
	require.NoError(t, err)

	err = s.AssignBackendToAffinityGroup(ctx, group.ID, backend.ID)
	require.NoError(t, err)

	backends, err := s.ListBackendsForAffinityGroup(ctx, group.ID)
	require.NoError(t, err)
	require.Len(t, backends, 1)

	err = s.RemoveBackendFromAffinityGroup(ctx, group.ID, backend.ID)
	require.NoError(t, err)

	backends, err = s.ListBackendsForAffinityGroup(ctx, group.ID)
	require.NoError(t, err)
	require.Empty(t, backends)
}

func TestUnit_groups_ListgroupsForBackend(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	backend := &runtimetypes.Backend{ID: uuid.NewString(), Name: "Backend1"}
	err := s.CreateBackend(ctx, backend)
	require.NoError(t, err)

	group1 := &runtimetypes.AffinityGroup{ID: uuid.NewString(), Name: "group1"}
	group2 := &runtimetypes.AffinityGroup{ID: uuid.NewString(), Name: "group2"}
	err = s.CreateAffinityGroup(ctx, group1)
	require.NoError(t, err)
	err = s.CreateAffinityGroup(ctx, group2)
	require.NoError(t, err)

	err = s.AssignBackendToAffinityGroup(ctx, group1.ID, backend.ID)
	require.NoError(t, err)
	err = s.AssignBackendToAffinityGroup(ctx, group2.ID, backend.ID)
	require.NoError(t, err)

	groups, err := s.ListAffinityGroupsForBackend(ctx, backend.ID)
	require.NoError(t, err)
	require.Len(t, groups, 2)
	groupIDs := map[string]bool{group1.ID: true, group2.ID: true}
	for _, p := range groups {
		require.True(t, groupIDs[p.ID])
	}
}

func TestUnit_groupModel_AssignModelTogroup(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	model := &runtimetypes.Model{
		ID:            uuid.New().String(),
		Model:         "test-model",
		ContextLength: 4096,
		CanChat:       true,
		CanEmbed:      false,
		CanPrompt:     true,
		CanStream:     false,
	}
	require.NoError(t, s.AppendModel(ctx, model))

	group := &runtimetypes.AffinityGroup{
		ID:          uuid.New().String(),
		Name:        "test-group",
		PurposeType: "test-purpose",
	}
	require.NoError(t, s.CreateAffinityGroup(ctx, group))

	require.NoError(t, s.AssignModelToAffinityGroup(ctx, group.ID, model.ID))

	models, err := s.ListModelsForAffinityGroup(ctx, group.ID)
	require.NoError(t, err)
	require.Len(t, models, 1)

	require.Equal(t, model.ID, models[0].ID)
	require.Equal(t, "test-model", models[0].Model)
	require.Equal(t, 4096, models[0].ContextLength)
	require.True(t, models[0].CanChat)
	require.False(t, models[0].CanEmbed)
	require.True(t, models[0].CanPrompt)
	require.False(t, models[0].CanStream)
	require.WithinDuration(t, model.CreatedAt, models[0].CreatedAt, time.Second)
	require.WithinDuration(t, model.UpdatedAt, models[0].UpdatedAt, time.Second)
}

func TestUnit_groups_RemoveModelFromgroup(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	model := &runtimetypes.Model{Model: "model1", ContextLength: 1024, CanPrompt: true, CanStream: false}
	err := s.AppendModel(ctx, model)
	require.NoError(t, err)

	group := &runtimetypes.AffinityGroup{ID: uuid.NewString(), Name: "group1"}
	err = s.CreateAffinityGroup(ctx, group)
	require.NoError(t, err)

	err = s.AssignModelToAffinityGroup(ctx, group.ID, model.ID)
	require.NoError(t, err)

	models, err := s.ListModelsForAffinityGroup(ctx, group.ID)
	require.NoError(t, err)
	require.Len(t, models, 1)

	err = s.RemoveModelFromAffinityGroup(ctx, group.ID, model.ID)
	require.NoError(t, err)

	models, err = s.ListModelsForAffinityGroup(ctx, group.ID)
	require.NoError(t, err)
	require.Empty(t, models)
}

func TestUnit_groups_ListgroupsForModel(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	model := &runtimetypes.Model{Model: "model1", ContextLength: 1024, CanPrompt: true, CanStream: false}
	err := s.AppendModel(ctx, model)
	require.NoError(t, err)

	group1 := &runtimetypes.AffinityGroup{ID: uuid.NewString(), Name: "group1"}
	group2 := &runtimetypes.AffinityGroup{ID: uuid.NewString(), Name: "group2"}
	err = s.CreateAffinityGroup(ctx, group1)
	require.NoError(t, err)
	err = s.CreateAffinityGroup(ctx, group2)
	require.NoError(t, err)

	err = s.AssignModelToAffinityGroup(ctx, group1.ID, model.ID)
	require.NoError(t, err)
	err = s.AssignModelToAffinityGroup(ctx, group2.ID, model.ID)
	require.NoError(t, err)

	groups, err := s.ListAffinityGroupsForModel(ctx, model.ID)
	require.NoError(t, err)
	require.Len(t, groups, 2)
	groupIDs := map[string]bool{group1.ID: true, group2.ID: true}
	for _, p := range groups {
		require.True(t, groupIDs[p.ID])
	}
}

func TestUnit_groups_GetNonExistentgroup(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	_, err := s.GetAffinityGroup(ctx, uuid.NewString())
	require.ErrorIs(t, err, libdb.ErrNotFound)

	_, err = s.GetAffinityGroupByName(ctx, "non-existent")
	require.ErrorIs(t, err, libdb.ErrNotFound)
}

func TestUnit_groups_DuplicategroupName(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	group := &runtimetypes.AffinityGroup{ID: uuid.NewString(), Name: "Duplicate"}
	err := s.CreateAffinityGroup(ctx, group)
	require.NoError(t, err)

	group2 := &runtimetypes.AffinityGroup{ID: uuid.NewString(), Name: "Duplicate"}
	err = s.CreateAffinityGroup(ctx, group2)
	require.Error(t, err)
}

// TestUnit_groups_ListEmptyAssociations verifies that listing associations
// for a new resource correctly returns an empty slice, not nil.
func TestUnit_groups_ListEmptyAssociations(t *testing.T) {
	ctx, s := runtimetypes.SetupStore(t)

	group := &runtimetypes.AffinityGroup{ID: uuid.NewString(), Name: "Emptygroup"}
	err := s.CreateAffinityGroup(ctx, group)
	require.NoError(t, err)

	backends, err := s.ListBackendsForAffinityGroup(ctx, group.ID)
	require.NoError(t, err)
	require.NotNil(t, backends, "ListBackendsForgroup should return an empty slice, not nil")
	require.Len(t, backends, 0)

	models, err := s.ListModelsForAffinityGroup(ctx, group.ID)
	require.NoError(t, err)
	require.NotNil(t, models, "ListModelsForgroup should return an empty slice, not nil")
	require.Len(t, models, 0)

	backend := &runtimetypes.Backend{ID: uuid.NewString(), Name: "EmptyBackend"}
	err = s.CreateBackend(ctx, backend)
	require.NoError(t, err)

	groups, err := s.ListAffinityGroupsForBackend(ctx, backend.ID)
	require.NoError(t, err)
	require.NotNil(t, groups, "ListgroupsForBackend should return an empty slice, not nil")
	require.Len(t, groups, 0)

	model := &runtimetypes.Model{Model: "empty-model", ContextLength: 1024, CanChat: true}
	err = s.AppendModel(ctx, model)
	require.NoError(t, err)

	groupsForModel, err := s.ListAffinityGroupsForModel(ctx, model.ID)
	require.NoError(t, err)
	require.NotNil(t, groupsForModel, "ListgroupsForModel should return an empty slice, not nil")
	require.Len(t, groupsForModel, 0)
}
