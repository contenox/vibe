---
title: "vald-operator: a Kubernetes operator for per-tenant vector search"
description: A Kubernetes controller that turned one declarative object into a full vector-search cluster per tenant, with its unsafe fields made immutable at the API boundary itself.
---

# vald-operator: a Kubernetes operator for per-tenant vector search

vald-operator provisioned a dedicated vector-search cluster for every tenant that needed one — one cluster per tenant-and-embedding-dimension pair — from a single custom resource. Create the object, and the operator rendered it into the underlying vector-search release, watched the resulting workloads until they reported ready, and published the live gateway address back to the platform that asked for it — provisioning infrastructure reduced to the same create-and-watch shape as any other Kubernetes object.

The fields that could silently corrupt a live index if changed — dimension, distance metric, object type — were declared immutable and enforced by the Kubernetes API server itself, through CEL validation on the resource's own schema, so no separate admission webhook was needed and no code path could accidentally slip a mutation through. Changing an embedding model was never an in-place edit; it meant standing up a second cluster and cutting over. The operator's own scope stayed deliberately narrow — provision and observe, not rebalance or drain — and it was proven at three test tiers: pure unit tests, a real API server via envtest, and a full kind cluster driving a live resource to ready.


Provisioned infrastructure surfaced back in the operator console, where worker pools and hosted app instances were tracked per deployment.

![The operator console's worker-pools view](/lab-admin-bob-worker-pools.png)

![The operator console's hosted-apps view, where an OCI chart becomes a per-tenant instance](/lab-admin-bob-apps.png)

## What it proved

- **Modeling a piece of infrastructure as one declarative custom resource, reconciled the same way Kubernetes reconciles everything else, kept per-tenant provisioning boring, observable, and easy to test.**
- **CEL validation at the CRD level closed off an entire bug class** — an in-place edit that would quietly corrupt a live index — before it could ever reach a reconcile loop.
- **Keeping the scope to provision-and-observe, deliberately deferring anything riskier, was what let the operator actually finish:** unit, real-API-server, and real-cluster tests all green.

## Where this lives now

vald-operator's cluster-per-tenant model has no direct successor in a local-first harness with no cluster to run — but its instinct, that a piece of infrastructure should be one declared, addressable object rather than a set of manual steps, still shows up in how the runtime treats a registered backend: added once by name, and checked with `contenox doctor` rather than hand-verified.
