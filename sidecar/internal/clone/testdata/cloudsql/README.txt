Cloud SQL Admin API v1 responses for the cloudsql_clone clone provider tests (lab-05e).

They follow the documented response shapes of the Cloud SQL Admin API reference
(https://cloud.google.com/sql/docs/postgres/admin-api/rest/v1/instances and
.../v1/operations): GET /v1/projects/{project}/instances/{instance},
POST /v1/projects/{project}/instances/{instance}/clone,
GET /v1/projects/{project}/operations/{operation},
PATCH and DELETE /v1/projects/{project}/instances/{instance}. The project,
names, IPs and operation ids are invented; no live project was called. Fields
the provider does not read are kept so a decoder that rejects unknown fields
would fail here.
