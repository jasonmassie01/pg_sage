import { useEffect, useRef } from 'react'
import {
  DeploymentDetail, DeploymentList, ProvisionForm, RequestQueue,
} from './AgentDBSections'
import {
  ProfilePanel, ProviderReadinessPanel,
} from './AgentDBProvisioningPanels'
import { ProviderSettingsPanel } from './ProviderSettingsPanel'
import { TerraformTemplatePanel } from './TerraformTemplatePanel'
import { BlueprintBuilderPanel } from './BlueprintBuilderPanel'

const CLOUD_SETUP_DOC_URL = [
  'https://github.com/jasonmassie01/pg_sage/blob/master/docs/runbooks/',
  'agentdb-cloud-provider-setup.md',
].join('')

const TAB_DESCRIPTIONS = {
  deployments: [
    'Track every agent-created database, inspect its cost, backup, tuning,',
    'promotion, and audit evidence, then run safe lifecycle actions.',
  ].join(' '),
  provision: [
    'Request a new agent database with the right provider, isolation level,',
    'lease, budget, workload hints, extensions, and Lakebase branch source.',
  ].join(' '),
  profiles: [
    'Create reusable t-shirt sizes that map agent needs to local capacity or',
    'cloud-specific instance, backup, networking, and extension parameters.',
  ].join(' '),
  'provider-settings': [
    'Control which cloud providers may execute live operations, constrain',
    'regions and accounts, and keep secret-bearing settings out of responses.',
  ].join(' '),
  terraform: [
    'Upload and review policy-checked Terraform templates for customized',
    'provider shapes without letting uploaded code apply directly.',
  ].join(' '),
  blueprints: [
    'Turn an English deployment intent into a typed AgentDB blueprint and',
    'draft Terraform template for policy review.',
  ].join(' '),
  activity: [
    'Review provision requests and policy decisions so operators can see why',
    'agent database work was approved, blocked, or queued.',
  ].join(' '),
}

export function AgentDBWorkspace({
  activeTab,
  form,
  profileForm,
  busy,
  deployments,
  selected,
  selectedID,
  detailFocusKey,
  detail,
  requests,
  profiles,
  providers,
  providerConfigs,
  terraformTemplates,
  blueprints,
  onFormChange,
  onProfileFormChange,
  onSubmitProvision,
  onSubmitProfile,
  onLifecycle,
  onProvisionAction,
  onBackupCheck,
  onRestoreDrillDryRun,
  onMarkRestoreVerified,
  onCreateDeployRequest,
  onReviewDeployRequest,
  onRequestDeployReview,
  onProvisionApprovedRequest,
  onSaveProviderSettings,
  onUploadTerraformTemplate,
  onApproveTerraformTemplate,
  onProvisionTerraformTemplate,
  onGenerateBlueprint,
  onApproveBlueprint,
  onProvisionBlueprint,
  onSelectDeployment,
}) {
  return (
    <div role="tabpanel"
      id={`agent-db-panel-${activeTab}`}
      aria-labelledby={`agent-db-tab-${activeTab}`}>
      <p className="mb-3 text-sm leading-6"
        data-testid="agent-db-tab-description"
        style={{ color: 'var(--text-secondary)' }}>
        {TAB_DESCRIPTIONS[activeTab]}
        {' '}
        {showCloudSetupLink(activeTab) && (
          <a href={CLOUD_SETUP_DOC_URL} target="_blank" rel="noreferrer"
            className="underline"
            style={{ color: 'var(--accent)' }}>
            Cloud provider setup
          </a>
        )}
      </p>
      {activeTab === 'deployments' && (
        <DeploymentsTab
          deployments={deployments}
          selected={selected}
          selectedID={selectedID}
          detailFocusKey={detailFocusKey}
          detail={detail}
          busy={busy}
          onSelectDeployment={onSelectDeployment}
          onLifecycle={onLifecycle}
          onProvisionAction={onProvisionAction}
          onBackupCheck={onBackupCheck}
          onRestoreDrillDryRun={onRestoreDrillDryRun}
          onMarkRestoreVerified={onMarkRestoreVerified}
          onCreateDeployRequest={onCreateDeployRequest}
          onReviewDeployRequest={onReviewDeployRequest}
          onRequestDeployReview={onRequestDeployReview}
        />
      )}
      {activeTab === 'provision' && (
        <ProvisionForm
          form={form}
          busy={busy}
          profiles={profiles}
          onChange={onFormChange}
          onSubmit={onSubmitProvision}
        />
      )}
      {activeTab === 'profiles' && (
        <ProfilesTab
          profiles={profiles}
          providers={providers}
          form={profileForm}
          busy={busy}
          onChange={onProfileFormChange}
          onSubmit={onSubmitProfile}
        />
      )}
      {activeTab === 'provider-settings' && (
        <ProviderSettingsPanel
          configs={providerConfigs}
          providers={providers}
          busy={busy}
          onSave={onSaveProviderSettings}
          docsUrl={CLOUD_SETUP_DOC_URL}
        />
      )}
      {activeTab === 'terraform' && (
        <TerraformTemplatePanel
          templates={terraformTemplates}
          busy={busy}
          onUpload={onUploadTerraformTemplate}
          onApprove={onApproveTerraformTemplate}
          onProvision={onProvisionTerraformTemplate}
          docsUrl={CLOUD_SETUP_DOC_URL}
        />
      )}
      {activeTab === 'blueprints' && (
        <BlueprintBuilderPanel
          blueprints={blueprints}
          busy={busy}
          onGenerate={onGenerateBlueprint}
          onApprove={onApproveBlueprint}
          onProvision={onProvisionBlueprint}
          docsUrl={CLOUD_SETUP_DOC_URL}
        />
      )}
      {activeTab === 'activity' && (
        <RequestQueue
          requests={requests}
          busy={busy}
          onProvisionApproved={onProvisionApprovedRequest}
        />
      )}
    </div>
  )
}

function showCloudSetupLink(activeTab) {
  return [
    'provision',
    'profiles',
    'provider-settings',
    'terraform',
    'blueprints',
  ].includes(activeTab)
}

function DeploymentsTab({
  deployments,
  selected,
  selectedID,
  detailFocusKey,
  detail,
  busy,
  onSelectDeployment,
  onLifecycle,
  onProvisionAction,
  onBackupCheck,
  onRestoreDrillDryRun,
  onMarkRestoreVerified,
  onCreateDeployRequest,
  onReviewDeployRequest,
  onRequestDeployReview,
}) {
  const detailRef = useRef(null)
  const lastFocusKeyRef = useRef(0)

  useEffect(() => {
    // Only scroll/focus on an explicit selection or focus action (which bumps
    // detailFocusKey). Auto-reselection after a delete changes selectedID
    // without bumping the key and must not steal focus or move the viewport.
    if (lastFocusKeyRef.current === detailFocusKey) return
    lastFocusKeyRef.current = detailFocusKey
    if (!detailFocusKey || !selectedID || !detailRef.current) return
    if (typeof detailRef.current.scrollIntoView === 'function') {
      detailRef.current.scrollIntoView({ block: 'start', behavior: 'smooth' })
    }
    detailRef.current.focus({ preventScroll: true })
  }, [detailFocusKey, selectedID])

  return (
    <div className="grid gap-4 lg:grid-cols-[minmax(280px,360px)_minmax(0,1fr)]">
      <DeploymentList
        deployments={deployments}
        selectedID={selectedID}
        onSelect={onSelectDeployment}
        onLifecycle={onLifecycle}
      />
      <div ref={detailRef} tabIndex={-1}
        className="min-w-0 scroll-mt-4 outline-none lg:sticky lg:top-4 lg:self-start">
        <DeploymentDetail
          deployment={selected}
          detail={detail}
          busy={busy}
          onProvisionPreflight={id => onProvisionAction(id, 'preflight')}
          onProvisionExecute={id => onProvisionAction(id, 'execute')}
          onProvisionExecuteLive={id => onProvisionAction(id, 'live-execute')}
          onProvisionStatus={id => onProvisionAction(id, 'status')}
          onProvisionDestroyDryRun={id =>
            onProvisionAction(id, 'destroy-dry-run')}
          onProvisionDestroyLive={id => onProvisionAction(id, 'destroy-live')}
          onBackupCheck={onBackupCheck}
          onRestoreDrillDryRun={onRestoreDrillDryRun}
          onMarkRestoreVerified={onMarkRestoreVerified}
          onCreateDeployRequest={onCreateDeployRequest}
          onApproveDeployRequest={(id, requestID) =>
            onReviewDeployRequest(id, requestID, 'approve')}
          onDenyDeployRequest={(id, requestID) =>
            onReviewDeployRequest(id, requestID, 'deny')}
          onRequestDeployReview={onRequestDeployReview}
        />
      </div>
    </div>
  )
}

function ProfilesTab({
  profiles,
  providers,
  form,
  busy,
  onChange,
  onSubmit,
}) {
  return (
    <div className="grid gap-4 xl:grid-cols-[minmax(0,1fr)_360px]">
      <ProfilePanel
        profiles={profiles}
        form={form}
        busy={busy}
        onChange={onChange}
        onSubmit={onSubmit}
      />
      <ProviderReadinessPanel providers={providers} />
    </div>
  )
}
