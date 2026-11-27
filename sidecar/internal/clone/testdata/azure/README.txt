Azure Resource Manager responses for the azure_restore clone provider tests (lab-05g).

They follow the documented response shapes of the Microsoft.DBforPostgreSQL
flexibleServers REST reference, api-version 2022-12-01
(https://learn.microsoft.com/rest/api/postgresql/flexibleserver/servers and the
ARM asynchronous operations guide): GET, PUT (createMode PointInTimeRestore)
and DELETE /subscriptions/{s}/resourceGroups/{g}/providers/
Microsoft.DBforPostgreSQL/flexibleServers/{server}, and GET of the
Azure-AsyncOperation URL /subscriptions/{s}/providers/Microsoft.DBforPostgreSQL/
locations/{location}/azureAsyncOperation/{id}. The subscription, names, keys and
operation ids are invented; no live subscription was called. Fields the
provider does not read are kept so a decoder that rejects unknown fields would
fail here.
