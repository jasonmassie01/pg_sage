package provider

// AdminRolePattern matches the managed-service administration logins
// (RDS, Cloud SQL, Azure, AlloyDB). pg_sage reports nothing about them
// and never budgets or alters them.
const AdminRolePattern = `^(rdsadmin|rdsrepladmin|rds_|cloudsqladmin|cloudsqlreplica|` +
	`azure_superuser|azuresu|alloydbadmin|alloydbreplica)`
