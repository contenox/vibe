package runtimetypes

// LocalTenantID is the single-tenant identity used by the local runtime. It is
// passed explicitly to every tenant-scoped API (vfsservice, vfsstore,
// runtimestate.EnsureModels) so that embedders layering multi-tenancy
// on top can substitute real tenant values without forking.
const LocalTenantID = "00000000-0000-0000-0000-000000000001"
