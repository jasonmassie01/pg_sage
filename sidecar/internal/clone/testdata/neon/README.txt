Neon API v2 responses for the neon_branch clone provider tests (lab-05b).

They follow the documented response shapes of the Neon API reference
(https://api-docs.neon.tech/reference/getting-started-with-neon-api):
GET /projects/{project_id}/endpoints, GET /projects/{project_id}/branches,
POST /projects/{project_id}/branches, GET /projects/{project_id}/operations/{id},
GET /projects/{project_id}/connection_uri, GET and DELETE
/projects/{project_id}/branches/{branch_id}. Ids, hosts and the password are
invented; no live account was called. Fields the provider does not read are
kept so a decoder that rejects unknown fields would fail here.
