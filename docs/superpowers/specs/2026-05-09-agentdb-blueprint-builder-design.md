# AgentDB Blueprint Builder Design

## Goal

Add an AgentDB Blueprint Builder that turns a short English deployment intent
into a typed database infrastructure blueprint and a draft Terraform template.
The LLM may translate intent, but pg_sage remains the authority for policy,
validation, persistence, review, and provisioning gates.

## Product Shape

The operator or agent submits a few paragraphs such as:

> Create a private AWS RDS Postgres database for a support agent in us-east-1.
> It needs Multi-AZ HA, PITR, 7-day backups, pgvector and PostGIS, a medium
> instance class, 100 GB storage, no public IP, and a $400 monthly budget.

pg_sage returns:

- a normalized blueprint JSON document,
- generated Terraform files,
- Terraform static-policy findings,
- pg_sage policy findings,
- the generated Terraform template ID,
- whether an LLM or deterministic fallback produced the blueprint.

The generated Terraform is always a draft. It is never applied directly by the
LLM path. Existing AgentDB live provisioning gates still control live execution.

## Architecture

The path is:

English intent -> `BlueprintSpec` -> Terraform files -> template validation ->
stored blueprint -> review/approve/apply through existing AgentDB flows.

The backend stores blueprints in `sage.agent_db_blueprints`. Each blueprint
links to an entry in `sage.agent_db_terraform_templates`. The generated
template uses `source_kind = blueprint` and can later be approved with the
existing Terraform template approval endpoint.

The LLM boundary is an interface:

```go
type BlueprintGenerator interface {
    GenerateBlueprint(context.Context, BlueprintDraftRequest) (BlueprintGeneration, error)
}
```

If no LLM is available, pg_sage uses a deterministic parser that covers common
provider, HA, backup, network, extension, budget, region, and instance-size
phrases. This keeps local QA and tests stable while allowing live LLM extraction
in configured deployments.

## Safety

The LLM cannot choose to apply infrastructure. It can only propose a typed
blueprint. pg_sage then:

- validates provider and provisioning level,
- rejects public IP unless explicitly allowed by policy,
- checks minimum backup/PITR expectations for HA or production-like requests,
- records unsupported or risky extension requests,
- redacts generated files with existing Terraform secret scrubbing,
- runs existing Terraform template static checks,
- stores audit-friendly status and policy findings.

## UI

Add a `Blueprints` tab to AgentDB. The page contains:

- intent textarea,
- provider selector,
- created-by field,
- submit button,
- generated blueprint cards with status, provider, region, template ID,
  policy findings, and generated file names.

The current Terraform upload panel stays as the expert/manual path.

## API

Add:

- `GET /api/v1/agent-dbs/blueprints`
- `POST /api/v1/agent-dbs/blueprints`

The POST body includes:

```json
{
  "blueprint_id": "bp_support_agent",
  "name": "Support agent DB",
  "intent": "English paragraphs...",
  "provider": "aws_rds",
  "created_by": "operator"
}
```

The response includes the stored blueprint and generated template ID.

## Exit Criteria

- Unit tests cover deterministic blueprint extraction and Terraform generation.
- Store/API tests cover blueprint persistence and template linkage.
- UI tests cover blueprint submission and rendering.
- Existing Terraform upload still works, using `body` consistently for file
  content.
- Full Go and web test/build gates pass.
