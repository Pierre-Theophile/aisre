// SPDX-License-Identifier: Apache-2.0

package sanitise

import "strings"

// The GCP connector's payloads, as its live poller writes them (003 T186, FR-134, FR-135, FR-137).
//
// Every row is rooted at `gcp.<payload kind>`, which is what Sanitiser.JSON addresses a payload's
// fields by, so none of them can answer for another source's field. The rows were written from the
// field paths of a real staging project's payloads (2026-10-09), read without their values; a field
// GCP adds later has no row, and a payload carrying it is refused whole rather than recorded with a
// guess (FR-134).
//
// The rules, in the order a reviewer should argue with them:
//
//   - People are dropped: a creator, a last modifier, an operation's user, every principal of the
//     audit stream. They are never hashed (FR-135).
//   - Free text is dropped: condition messages, a policy's documentation, a poll's failure detail,
//     container arguments, annotations.
//   - Configuration VALUES are dropped (FR-034): a request or response body in the audit stream, and an
//     environment variable's value — except the single Cloud SQL connection name the GCP pre-pass
//     reduces a value to, which is an identifier rather than a value and is pseudonymised as the
//     volume field is. Variable NAMES are kept, as the deploy feeders keep a key: the name is the
//     configuration change's subject.
//   - Identifiers are pseudonymised in the shape their field is parsed in. Projects and Cloud SQL
//     instances take ShapeLabel, because a connection name `<project>:<region>:<instance>` is parsed
//     by a pattern a token's underscores fail. Services, revisions and the rest take the token, which
//     is the shape the `service` label already has, so a label and the service it names still join.
//   - Locations are kept verbatim, through `{region}`: a region, a zone or `global` is the
//     platform's geography and the connector validates it by shape.
//   - A commit takes ShapeHex under KindCommit, the shape the deploy feeders give the same commit, so
//     C8 still merges a Cloud Run rollout with the GitHub deployment that shipped it.
//   - Monitoring filters are kept REDUCED: the GCP pre-pass keeps only the equality clauses the feeder
//     reads, with every identifier pseudonymised, so a recorded alert policy still watches the
//     recorded service. An investigation starts from what an alert watches.
func addGCPPayloadRows(p *Policy) {
	set := func(path string, rule Rule) { p.fields[CanonicalField(path)] = rule }
	drop := func(path, why string) { set(path, Rule{Disposition: Dropped, Why: why}) }
	keep := func(path, why string) { set(path, Rule{Disposition: Verbatim, Why: why}) }
	pseudo := func(path string, kind Kind, sh Shape, why string) {
		set(path, Rule{Disposition: Pseudonym, Kind: kind, Shape: sh, Why: why})
	}
	tmpl := func(path, pattern, why string) {
		set(path, Rule{Disposition: Pseudonym, Kind: KindResource, Shape: ShapeTemplate, Template: pattern, Why: why})
	}
	keepAll := func(prefix, why string, fields ...string) {
		for _, f := range fields {
			keep(prefix+"."+f, why)
		}
	}

	const (
		project    = "projects/{project:label}"
		service    = project + "/locations/{region}/services/{service}"
		revision   = service + "/revisions/{revision}"
		instance   = project + "/instances/{instance:label}"
		connection = "{project:label}:{region}:{instance:label}"
		cluster    = project + "/locations/{region}/clusters/{cluster}"
		whyTime    = "an instant the platform states"
		whyEnum    = "a closed platform value"
		whyCount   = "a platform setting or counter, naming nothing"
	)
	resourceName := strings.Join([]string{revision, service, "namespaces/{project:label}/services/{service}",
		instance, cluster, project}, TemplateAlternatives)

	// labels: the allowlist's rules (config/gcp.yaml), at each path a label map sits, and every other
	// key dropped by the open-namespace rule.
	labels := func(prefix string) {
		drop(prefix+".labels.*", "a label not on the allowlist (config/gcp.yaml)")
		for key, rule := range p.labels {
			set(prefix+".labels."+key, rule)
		}
		pseudo(prefix+".labels.commit-sha", KindCommit, ShapeHex, "a commit; hex, as the deploy feeders shape it, so C8 still joins")
		drop(prefix+".annotations.*", "free text, and keyed by whoever set it")
	}

	// --- Cloud Run: what a service's template and a revision share ------------------------------
	container := func(c string) {
		drop(c+".args[]", "container arguments: free text that routinely carries a secret")
		drop(c+".command[]", "the same")
		drop(c+".baseimageuri", "a build input's location")
		drop(c+".buildinfo.*", "a source location")
		tmpl(c+".image", imagePatterns(), "the image; its digest is the deploy.image join, kept as hex of its own length, "+
			"and a tag is a release token. A registry or path shape not listed is dropped")
		pseudo(c+".name", KindResource, ShapeToken, "a container name, usually the service's")
		keep(c+".env[].name", "a variable's NAME: the configuration change's subject, as the deploy feeders keep a key")
		tmpl(c+".env[].value", connection, "the one Cloud SQL connection name the pre-pass reduced the value to, "+
			"pseudonymised as the volume field is; FR-028's env_var_value edge rests on it. Any other value never "+
			"reaches this table (FR-034 as amended)")
		pseudo(c+".env[].valuesource.secretkeyref.secret", KindResource, ShapeToken, "a secret's name; joined to the secret version it reads")
		keep(c+".env[].valuesource.secretkeyref.version", "a secret version number or `latest`")
		for _, probe := range []string{"livenessprobe", "startupprobe", "readinessprobe"} {
			keepAll(c+"."+probe, whyCount, "failurethreshold", "periodseconds", "timeoutseconds", "initialdelayseconds",
				"httpget.port", "tcpsocket.port", "grpc.port")
			drop(c+"."+probe+".httpget.path", "a URL path, free text")
			drop(c+"."+probe+".httpget.httpheaders[].*", "header values, free text")
		}
		keepAll(c, whyCount, "ports[].containerport", "ports[].name", "resources.cpuidle", "resources.limits.cpu",
			"resources.limits.memory", "resources.startupcpuboost", "workingdir")
		keep(c+".volumemounts[].mountpath", "where configuration is mounted; FR-034's mounts property")
		pseudo(c+".volumemounts[].name", KindResource, ShapeToken, "a volume name; joined to the volume it mounts")
		drop(c+".dependson[]", "container names, which the name row above pseudonymises separately")
	}
	revisionTemplate := func(r string) {
		container(r + ".containers[]")
		pseudo(r+".volumes[].name", KindResource, ShapeToken, "a volume name; joined to its mount")
		tmpl(r+".volumes[].cloudsqlinstance.instances[]", connection, "the attached instance's connection name; FR-028's edge rests on it")
		pseudo(r+".volumes[].gcs.bucket", KindResource, ShapeToken, "a bucket name")
		drop(r+".volumes[].gcs.mountoptions[]", "free text")
		pseudo(r+".volumes[].secret.secret", KindResource, ShapeToken, "a secret's name; the same token its env reference gets")
		drop(r+".volumes[].secret.items[].path", "a file path chosen by whoever mounted it")
		keep(r+".volumes[].secret.items[].version", "a secret version number or `latest`")
		keep(r+".volumes[].emptydir.medium", whyEnum)
		keep(r+".volumes[].emptydir.sizelimit", whyCount)
		tmpl(r+".serviceaccount", "{resource}@{project:label}.iam.gserviceaccount.com",
			"the identity the revision runs as; an identity, not a credential. A default compute account names a project number and is dropped")
		keep(r+".vpcaccess.egress", whyEnum)
		tmpl(r+".vpcaccess.connector", project+"/locations/{region}/connectors/{resource}", "a VPC connector")
		tmpl(r+".vpcaccess.networkinterfaces[].network", "{resource}"+TemplateAlternatives+project+"/global/networks/{resource}", "a network")
		tmpl(r+".vpcaccess.networkinterfaces[].subnetwork", "{resource}"+TemplateAlternatives+project+"/regions/{region}/subnetworks/{resource}", "a subnetwork")
		keepAll(r, whyCount, "scaling.maxinstancecount", "scaling.mininstancecount", "maxinstancerequestconcurrency",
			"timeout", "executionenvironment", "sessionaffinity", "encryptionkey")
		labels(r)
	}

	// --- gcp.services ----------------------------------------------------------------------------
	const s = "gcp.services.services[]"
	tmpl(s+".name", service, "the service's resource name")
	revisionTemplate(s + ".template")
	pseudo(s+".template.revision", KindRevision, ShapeToken, "the revision the template names; the token its resource name gets")
	labels(s)
	keepAll(s, "an opaque platform id or counter", "uid", "generation", "observedgeneration", "etag", "reconciling")
	keepAll(s, whyTime, "createtime", "updatetime", "deletetime", "expiretime")
	keepAll(s, whyEnum, "ingress", "launchstage", "client", "clientversion", "defaulturidisabled", "invokeriamdisabled")
	drop(s+".creator", "a person")
	drop(s+".lastmodifier", "a person")
	drop(s+".description", "free text")
	drop(s+".uri", "the service's address, a host name")
	drop(s+".urls[]", "the same")
	drop(s+".customaudiences[]", "addresses an operator chose")
	drop(s+".buildconfig.*", "build inputs: source locations and image URIs")
	keepAll(s, whyCount, "scaling.manualinstancecount", "scaling.maxinstancecount", "scaling.mininstancecount", "scaling.scalingmode")
	for _, c := range []string{s + ".conditions[]", s + ".terminalcondition"} {
		keepAll(c, whyEnum, "type", "state", "severity", "reason", "revisionreason", "executionreason")
		keep(c+".lasttransitiontime", whyTime)
		drop(c+".message", "free text")
	}
	keepAll(s+".traffic[]", whyEnum, "type", "percent")
	pseudo(s+".traffic[].revision", KindRevision, ShapeToken, "a revision name")
	pseudo(s+".traffic[].tag", KindResource, ShapeToken, "a traffic tag an operator chose")
	keepAll(s+".trafficstatuses[]", whyEnum, "type", "percent")
	pseudo(s+".trafficstatuses[].revision", KindRevision, ShapeToken, "a revision name; joined to the revision's resource name")
	pseudo(s+".trafficstatuses[].tag", KindResource, ShapeToken, "a traffic tag")
	drop(s+".trafficstatuses[].uri", "an address")
	tmpl(s+".latestreadyrevision", revision, "the revision serving")
	tmpl(s+".latestcreatedrevision", revision, "the newest revision")

	// --- gcp.revisions ---------------------------------------------------------------------------
	const r = "gcp.revisions.revisions[]"
	tmpl(r+".name", revision, "the revision's resource name")
	pseudo(r+".service", KindService, ShapeToken, "the service the revision belongs to")
	revisionTemplate(r)
	keepAll(r, "an opaque platform id or counter", "uid", "generation", "observedgeneration", "etag", "reconciling")
	keepAll(r, whyTime, "createtime", "updatetime", "deletetime", "expiretime")
	keepAll(r, whyEnum, "launchstage")
	keep(r+".scalingstatus.desiredmininstancecount", whyCount)
	drop(r+".creator", "a person")
	drop(r+".loguri", "a console link naming the project")
	for _, c := range []string{r + ".conditions[]"} {
		keepAll(c, whyEnum, "type", "state", "severity", "reason", "revisionreason", "executionreason")
		keep(c+".lasttransitiontime", whyTime)
		drop(c+".message", "free text")
	}

	// --- gcp.alert_policies ----------------------------------------------------------------------
	const a = "gcp.alert_policies.alertpolicies[]"
	policy := project + "/alertPolicies/{alert_policy}"
	tmpl(a+".name", policy, "the policy's resource name")
	drop(a+".displayname", "a display name: the contract treats every one as possibly naming a person")
	tmpl(a+".conditions[].name", policy+"/conditions/{resource}", "a condition's resource name")
	drop(a+".conditions[].displayname", "a display name, the same")
	// The filters arrive REDUCED by the GCP pre-pass (gcpfeeder.RecordingPrepare): only equality clauses
	// on the fields the feeder reads and closed vocabularies, every identifier already pseudonymised as
	// this table pseudonymises it elsewhere. Kept, because an investigation starts from what a policy
	// watches. The Datadog monitor query is the precedent (sanitise/datadog.go).
	for _, block := range []string{"conditionthreshold", "conditionmatchedlog", "conditionabsent"} {
		keep(a+".conditions[]."+block+".filter", "a filter the pre-pass reduced to pseudonymised equality clauses")
		keep(a+".conditions[]."+block+".denominatorfilter", "the same")
	}
	keepAll(a+".conditions[].conditionabsent", whyEnum, "duration", "trigger.count", "trigger.percent",
		"aggregations[].alignmentperiod", "aggregations[].crossseriesreducer", "aggregations[].perseriesaligner",
		"aggregations[].groupbyfields[]")
	drop(a+".conditions[].conditionmonitoringquerylanguage.query", "a query, the same")
	drop(a+".conditions[].conditionprometheusqueryLanguage.query", "a query, the same")
	keepAll(a+".conditions[].conditionthreshold", whyEnum, "comparison", "duration", "thresholdvalue", "trigger.count",
		"trigger.percent", "evaluationmissingdata", "aggregations[].alignmentperiod", "aggregations[].crossseriesreducer",
		"aggregations[].perseriesaligner", "aggregations[].groupbyfields[]")
	keepAll(a, whyEnum, "combiner", "enabled", "severity", "documentation.mimetype", "alertstrategy.autoclose",
		"alertstrategy.notificationprompts[]", "alertstrategy.notificationratelimit.period")
	keep(a+".creationrecord.mutatetime", whyTime)
	keep(a+".mutationrecord.mutatetime", whyTime)
	drop(a+".creationrecord.mutatedby", "a person")
	drop(a+".mutationrecord.mutatedby", "a person")
	drop(a+".documentation.*", "free text written for the on-call")
	tmpl(a+".notificationchannels[]", project+"/notificationChannels/{resource}", "a notification channel")
	drop(a+".userlabels.*", "labels an operator wrote; the allowlist is for resources, not policies")

	// --- gcp.alerts (the opt-in incident read) ---------------------------------------------------
	const al = "gcp.alerts.alerts[]"
	tmpl(al+".name", project+"/alerts/{resource}", "an incident's resource name")
	keepAll(al, whyEnum, "state")
	keepAll(al, whyTime, "opentime", "closetime")
	tmpl(al+".policy.name", policy, "the policy that fired")
	drop(al+".policy.displayname", "a display name, the same")
	keep(al+".policy.severity", whyEnum)
	keep(al+".resource.type", whyEnum)
	pseudo(al+".resource.labels.project_id", KindProject, ShapeLabel, "the project")
	pseudo(al+".resource.labels.service_name", KindService, ShapeToken, "the Cloud Run service")
	pseudo(al+".resource.labels.revision_name", KindRevision, ShapeToken, "the revision")
	tmpl(al+".resource.labels.location", "{region}", "a location")
	drop(al+".resource.labels.*", "a resource label this connector does not join on")
	drop(al+".metric.*", "the metric's labels, which carry values")
	drop(al+".policy.userlabels.*", "labels an operator wrote")
	drop(al+".log.*", "log content")

	// --- gcp.sql_flags: the public catalogue -----------------------------------------------------
	keepAll("gcp.sql_flags.items[]", "Google's published flag catalogue, the same for every customer",
		"allowedintvalues[]", "allowedstringvalues[]", "appliesto[]", "inbeta", "kind", "maxvalue", "minvalue", "name",
		"requiresrestart", "type", "flagscope", "recommendedintvalue", "recommendedstringvalue")

	// --- gcp.sql_instances -----------------------------------------------------------------------
	const i = "gcp.sql_instances.items[]"
	pseudo(i+".name", KindInstance, ShapeLabel, "the instance; label-shaped so a connection name naming it still parses")
	pseudo(i+".project", KindProject, ShapeLabel, "the project")
	tmpl(i+".connectionname", connection, "the instance connection name; C7 resolves on it")
	tmpl(i+".masterinstancename", "{project:label}:{instance:label}", "the primary a replica follows")
	pseudo(i+".replicanames[]", KindInstance, ShapeLabel, "a replica")
	tmpl(i+".selflink", "https://sqladmin.googleapis.com/sql/v1beta4/"+instance, "the instance's API link")
	tmpl(i+".region", "{region}", "a location")
	tmpl(i+".gcezone", "{region}", "a zone")
	tmpl(i+".secondarygcezone", "{region}", "a zone")
	keepAll(i, whyEnum, "backendtype", "databaseinstalledversion", "databaseversion", "instancetype", "kind", "state",
		"sqlnetworkarchitecture", "satisfiespzi", "satisfiespzs", "maintenanceversion", "availablemaintenanceversions[]",
		"replicaconfiguration.kind", "replicaconfiguration.failovertarget", "currentdisksize", "maxdisksize")
	keepAll(i, "an opaque platform tag or instant", "etag", "createtime")
	keepAll(i+".ipaddresses[]", whyEnum, "type")
	pseudo(i+".ipaddresses[].ipaddress", KindIPAddress, ShapeToken, "an infrastructure address (FR-135)")
	keep(i+".ipaddresses[].timetoretire", whyTime)
	drop(i+".dnsnames[].name", "a host name")
	drop(i+".dnsname", "a host name")
	keepAll(i+".dnsnames[]", whyEnum, "connectiontype", "dnsscope", "recordmanager")
	drop(i+".servercacert.*", "certificate material and its common name")
	drop(i+".serviceaccountemailaddress", "a Google-managed account naming the project number")
	drop(i+".replicationcluster.psawriteendpoint", "a host name")
	drop(i+".pscserviceattachmentlink", "a link naming the project")
	drop(i+".settings.userlabels.*", "labels an operator wrote on a database")
	drop(i+".settings.ipconfiguration.authorizednetworks[].name", "free text")
	pseudo(i+".settings.ipconfiguration.authorizednetworks[].value", KindIPAddress, ShapeToken, "an address range (FR-135)")
	keep(i+".settings.ipconfiguration.authorizednetworks[].kind", whyEnum)
	tmpl(i+".settings.ipconfiguration.privatenetwork", project+"/global/networks/{resource}", "the instance's network")
	drop(i+".settings.ipconfiguration.allocatediprange", "an operator-chosen range name")
	keep(i+".settings.databaseflags[].name", "a flag's name, from the public catalogue")
	keep(i+".settings.databaseflags[].value", "a flag's value: database configuration, which FR-030's config diff exists to show (`max_connections=500`)")
	tmpl(i+".settings.locationpreference.zone", "{region}", "a zone")
	tmpl(i+".settings.locationpreference.secondaryzone", "{region}", "a zone")
	tmpl(i+".settings.backupconfiguration.location", "{region}", "a backup location")
	keepAll(i+".settings", whyCount, "activationpolicy", "availabilitytype", "connectorenforcement", "crashsafereplicationenabled",
		"databasereplicationenabled", "datacacheconfig.datacacheenabled", "datadisksizegb", "datadisktype",
		"deletionprotectionenabled", "edition", "kind", "pricingplan", "replicationlagmaxseconds", "replicationtype",
		"settingsversion", "storageautoresize", "storageautoresizelimit", "tier",
		"backupconfiguration.backupretentionsettings.retainedbackups", "backupconfiguration.backupretentionsettings.retentionunit",
		"backupconfiguration.backuptier", "backupconfiguration.enabled", "backupconfiguration.kind",
		"backupconfiguration.pointintimerecoveryenabled", "backupconfiguration.replicationlogarchivingenabled",
		"backupconfiguration.starttime", "backupconfiguration.transactionallogstoragestate",
		"backupconfiguration.transactionlogretentiondays", "backupconfiguration.binarylogenabled",
		"insightsconfig.queryinsightsenabled", "insightsconfig.queryplansperminute", "insightsconfig.querystringlength",
		"insightsconfig.recordapplicationtags", "insightsconfig.recordclientaddress",
		"ipconfiguration.ipv4enabled", "ipconfiguration.requiressl", "ipconfiguration.servercamode",
		"ipconfiguration.servercertificaterotationmode", "ipconfiguration.sslmode", "ipconfiguration.enableprivatepathforgooglecloudservices",
		"locationpreference.kind", "maintenancewindow.day", "maintenancewindow.hour", "maintenancewindow.kind",
		"maintenancewindow.updatetrack", "sqlserverauditconfig.kind", "sqlserverauditconfig.retentioninterval",
		"sqlserverauditconfig.uploadinterval", "denymaintenanceperiods[].startdate", "denymaintenanceperiods[].enddate",
		"denymaintenanceperiods[].time")
	drop(i+".settings.sqlserverauditconfig.bucket", "a bucket name")

	// --- gcp.sql_operations ----------------------------------------------------------------------
	const o = "gcp.sql_operations.items[]"
	keepAll(o, "an operation's opaque id, type, state or instant", "name", "operationtype", "status", "kind",
		"inserttime", "starttime", "endtime", "suboperationtype.maintenancetype", "backupcontext.backupid", "backupcontext.kind",
		"error.kind", "error.errors[].code", "error.errors[].kind")
	pseudo(o+".targetid", KindInstance, ShapeLabel, "the instance operated on")
	pseudo(o+".targetproject", KindProject, ShapeLabel, "the project")
	tmpl(o+".targetlink", "https://sqladmin.googleapis.com/sql/v1beta4/"+instance, "the instance's API link")
	tmpl(o+".selflink", "https://sqladmin.googleapis.com/sql/v1beta4/"+project+"/operations/{resource}", "the operation's API link")
	drop(o+".user", "a person")
	drop(o+".error.errors[].message", "free text")
	drop(o+".backupcontext.name", "a backup's name, written by an operator")
	drop(o+".exportcontext.*", "an export's destination and databases")
	drop(o+".importcontext.*", "an import's source and databases")
	drop(o+".apiwarning.*", "free text")

	// --- gcp.gke_clusters ------------------------------------------------------------------------
	// Written without a cluster to read: the staging project the rows were taken from has none. A
	// cluster carrying a field below the rows is refused whole, which is FR-134's rule doing its job.
	const g = "gcp.gke_clusters.clusters[]"
	pseudo(g+".name", KindCluster, ShapeToken, "the cluster")
	tmpl(g+".location", "{region}", "a location")
	tmpl(g+".zone", "{region}", "a zone")
	tmpl(g+".selflink", "https://container.googleapis.com/v1/"+cluster, "the cluster's API link")
	keepAll(g, whyEnum, "status", "currentmasterversion", "currentnodeversion", "initialclusterversion",
		"releasechannel.channel", "autopilot.enabled", "etag", "id")
	keep(g+".createtime", whyTime)
	drop(g+".statusmessage", "free text")
	drop(g+".description", "free text")
	drop(g+".endpoint", "an address")
	drop(g+".nodepools.*", "inside the cluster; FR-030 never emits it")
	drop(g+".masterauth.*", "credential material")
	drop(g+".resourcelabels.*", "labels an operator wrote")
	drop(g+".locations[]", "zones, read from location instead")

	// --- gcp.audit -------------------------------------------------------------------------------
	const e = "gcp.audit.entries[]"
	keepAll(e, "GCP's duplicate key, an instant, or a closed value", "insertid", "timestamp", "receivetimestamp", "severity",
		"operation.id", "operation.producer", "operation.first", "operation.last", "resource.type")
	tmpl(e+".logname", project+"/logs/cloudaudit.googleapis.com%2Factivity", "the admin-activity stream; any other stream is dropped")
	pseudo(e+".resource.labels.project_id", KindProject, ShapeLabel, "the project")
	pseudo(e+".resource.labels.service_name", KindService, ShapeToken, "the Cloud Run service")
	pseudo(e+".resource.labels.revision_name", KindRevision, ShapeToken, "the revision")
	tmpl(e+".resource.labels.location", "{region}", "a location")
	tmpl(e+".resource.labels.region", "{region}", "a location")
	drop(e+".resource.labels.*", "a resource label naming something this connector does not join on")
	const pp = e + ".protopayload"
	keepAll(pp, "the API's own name for what happened", "@type", "servicename", "methodname", "status.code")
	tmpl(pp+".resourcename", resourceName, "the resource changed; any shape the connector does not parse is dropped")
	keepAll(pp+".authorizationinfo[]", "the permission checked and its outcome", "permission", "granted")
	tmpl(pp+".authorizationinfo[].resource", resourceName, "the resource authorised")
	drop(pp+".authorizationinfo[].resourceattributes.*", "attributes naming resources outside the parse")
	drop(pp+".authenticationinfo.*", "the principal and every delegation: people (FR-135)")
	drop(pp+".requestmetadata.*", "where the caller was and what they ran")
	drop(pp+".request.*", "the applied configuration; sensitive by FR-034 and unbounded in shape")
	drop(pp+".response.*", "the returned object, same reasoning")
	drop(pp+".metadata.*", "per-service detail, unbounded in shape")
	drop(pp+".resourceoriginalstate.*", "the previous object, same reasoning as the request")
	drop(pp+".resourcelocation.*", "locations, read from the resource name instead")
	drop(pp+".status.message", "free text")
	drop(pp+".status.details[]", "free-form error payloads")
	drop(e+".labels.*", "log labels, free text")
	drop(e+".trace", "a trace link naming the project")
	drop(e+".spanid", "the same")
	drop(e+".sourcelocation.*", "source code locations")
	drop(e+".httprequest.*", "a request's addresses and agent")

	// --- gcp.poll: the poller's own marker -------------------------------------------------------
	keepAll("gcp.poll", "the poll's outcome and the closed vocabularies its checkpoint carries", "outcome", "stop_reason", "deferred[]")
	// The reason is rewritten by the GCP pre-pass before this table sees it (feeders/gcp
	// PreparePayload): reduced to the names of the reads that failed, which are the published
	// operation names. As the poller writes it, it quotes projects and error text.
	keep("gcp.poll.reason", "the failed reads' operation names, after the pre-pass removed everything else")
	pseudo("gcp.poll.projects[]", KindProject, ShapeLabel, "a project in scope")
	tmpl("gcp.poll.regions[]", "{region}", "a region in scope")
	tmpl("gcp.poll.covered[]", "{project:label}/{region}", "a (project, region) pair read")
}

// imagePatterns are the image reference shapes a recording keeps, measured on a real project
// (2026-10-09): Artifact Registry `<location>-docker.pkg.dev/<project>/<repository>/<image>` and Container
// Registry `[<multi-region>.]gcr.io/<project>/<image>`, each pinned by digest or named by tag.
func imagePatterns() string {
	var registries []string
	registries = append(registries,
		"{region}-docker.pkg.dev/{project:label}/{repository}/{resource}",
		"{region}-docker.pkg.dev/{project:label}/{repository}/{resource}/{resource}")
	for _, host := range []string{"gcr.io", "eu.gcr.io", "us.gcr.io", "asia.gcr.io"} {
		registries = append(registries, host+"/{project:label}/{resource}", host+"/{project:label}/{resource}/{resource}")
	}
	var patterns []string
	for _, registry := range registries {
		patterns = append(patterns, registry+"@sha256:{commit:hex}", registry+":{release}")
	}
	return strings.Join(patterns, TemplateAlternatives)
}
