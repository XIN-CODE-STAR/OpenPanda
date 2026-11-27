// SPDX-License-Identifier: AGPL-3.0-or-later

import { useCallback, useEffect, useMemo, useRef, useState } from 'preact/hooks'
import {
  api,
  currentToken,
  type ModelRoster,
  type OnboardingPatch,
  type OnboardingState,
  type ProviderInfo,
} from '../api/client'
import { useLocaleRerender } from '../hooks'
import { loadLocale, locale, localeNames, locales, setLocale, t, type Locale } from '../i18n'
import { toast } from '../components/toast'
import { Icon } from '../components/icons'
import { TermsContent } from '../components/terms'
import { ModelPicker } from '../components/model-picker'
import { useModalFocus } from '../components/modal-focus'
import { ProviderCard } from '../components/provider-card'
import { navigate, parseHash } from '../nav'
import { onboardingGate } from '../utils/onboarding'
import { notifyModelSaved, onModelSaved } from '../model-events'
import { needsPlaintextConsent } from './model-endpoint'
import { GuideTour, type TourPhase } from './onboarding-guide'

// First-run onboarding: `panda web` boots zero-config — the panel is fully
// reachable but chat cannot answer until a model is set up, and the TUI
// additionally walks language → terms → approval before the first prompt.
// The browser includes a tour over the live console between consent and
// approvals. Both entry points share the saved onboarding configuration.

type Step = 'language' | 'terms' | 'guide' | 'approval' | 'model'
const STEPS: Step[] = ['language', 'terms', 'guide', 'approval', 'model']

/** First-run gate rendered at the top of the main pane. Fresh installs
 *  (no answer recorded yet) get the full wizard immediately; a recorded
 *  install whose consent is stale gets a persistent banner whose CTA opens
 *  the terms step. The gate keys off CONSENT, not wizard completion, but a
 *  dismissal is an answer too: re-opening the full wizard on every page load
 *  after the user clicked "not now" is nagging, so only a genuinely fresh
 *  install auto-opens. */
type TourControls = {
  onPhase(phase: TourPhase): void
}

/** The app shares the same onboarding snapshot across both gate surfaces. */
export function useOnboardingState(enabled: boolean) {
  const token = enabled ? currentToken() : ''
  const [state, setState] = useState<OnboardingState | null>(null)
  const revision = useRef(0)
  const acceptSnapshot = useCallback((result: OnboardingState) => {
    if (!token || token !== currentToken()) return
    // A GET begun before a saved change can finish after its response.
    ++revision.current
    setState(result)
  }, [token])
  useEffect(() => {
    setState(null)
    if (!token) return
    let alive = true
    async function check() {
      const mine = ++revision.current
      try {
        const result = await api.getOnboarding()
        if (alive && mine === revision.current) setState(result)
      } catch {
        // Keep the last successful state if a background refresh fails.
      }
    }
    void check()
    const unsubscribe = onModelSaved(check)
    return () => {
      alive = false
      unsubscribe()
    }
  }, [token])
  return [state, acceptSnapshot] as const
}

export function OnboardingBanner({ state, onState, onPhase }: {
  state: OnboardingState | null
  onState(result: OnboardingState): void
} & TourControls) {
  useLocaleRerender()
  const [open, setOpen] = useState(false)
  const dismissing = useRef(false)

  const gate = state ? onboardingGate(state) : null
  const termsOnly = gate?.termsOnly ?? false
  const banner = gate?.banner ?? 'none'
  // Only a genuinely fresh install opens without a click. A recorded install
  // with stale consent gets the banner; opening the modal every launch was
  // the "why does this keep popping up" complaint.
  useEffect(() => {
    if (!gate?.firstRun) return
    // The native Settings dialog would sit above the first-run wizard.
    if (parseHash().view === 'settings') navigate({ view: 'sessions', id: null }, true)
    setOpen(true)
  }, [gate?.firstRun])

  if (!state) return null

  // Closing the wizard is an explicit "not now" — record it so the next visit
  // shows the banner instead of marching through language → terms again. The
  // consent gate itself is unchanged: the banner stays until terms are current.
  async function dismiss() {
    if (dismissing.current) return
    dismissing.current = true
    try {
      onState(await api.postOnboarding({ onboarded: true }))
      setOpen(false)
    } catch (error) {
      toast(error instanceof Error ? error.message : String(error), 'error')
    } finally {
      dismissing.current = false
    }
  }

  // Only the consent case is still a bar across the top of the pane. The
  // no-model case moved to the rail's tail (`ModelNotice`): it is a standing
  // fact about this node — the same kind as "a newer build exists" — and it
  // belongs beside the node chip, not pushing every page down for a notice the
  // user has already read.
  const showBanner = !open && banner === 'terms'
  return (
    <>
      {showBanner && (
        <div class="onboarding-banner" role="status">
          <span class="onboarding-banner-text">{t('onboarding.termsBanner')}</span>
          <button class="btn primary small" onClick={() => setOpen(true)}>
            {t('onboarding.termsCta')}
          </button>
        </div>
      )}
      {open && (
        <OnboardingWizard
          state={state}
          termsOnly={termsOnly}
          onPhase={onPhase}
          onClose={dismiss}
          onDone={result => {
            onState(result)
            setOpen(false)
          }}
        />
      )}
    </>
  )
}

/** "No model configured" as a card in the rail's tail.
 *
 *  It used to be a full-width bar at the top of every view, which pushed the
 *  page down to say something the reader had already been told and could not
 *  act on from there anyway. It now sits where the update chip sits — directly
 *  above the node chip, at the rail's width — because both are standing facts
 *  about this node ("what I run" / "what I could run"), not news about the page
 *  underneath. The card takes the update chip's own material and the paused
 *  check's amber: a node with no model cannot answer, which is the same class
 *  of news as a check that gave up.
 *
 *  No dismiss: the condition clears itself the moment a model is saved, and a
 *  rail card is quiet enough not to nag. */
export function ModelNotice({ state }: { state: OnboardingState | null }) {
  useLocaleRerender()
  const gate = state ? onboardingGate(state) : null
  if (gate?.banner !== 'model') return null
  return (
    <div class="rail-update rail-update-model" role="status">
      <div class="rail-update-model-main">
        <Icon name="alert" class="rail-update-ico" />
        <div class="rail-update-model-text">
          <span class="rail-update-title">{t('ui.model.title')}</span>
          <p class="rail-update-sub">{t('ui.model.sub')}</p>
        </div>
      </div>
      <a class="rail-update-cta" href="#/settings/models">
        {t('onboarding.cta')}
      </a>
    </div>
  )
}

/** The first-run wizard, plus the terms-only pass. The model step lives at the
 *  end of the tour (a fresh install has no other place to land), but it is not
 *  a standalone destination: "already onboarded, still no model" is a settings
 *  job, and the rail's no-model card opens 设置 → 模型 instead of this modal. */
function OnboardingWizard(props: {
  state: OnboardingState
  termsOnly: boolean
  onClose(): void
  onDone(result: OnboardingState): void
} & TourControls) {
  useLocaleRerender()
  // Re-consent (termsOnly) must not re-run the tour: the install was already
  // configured, and this pass only collects a signature.
  const steps: Step[] = props.termsOnly ? ['terms'] : STEPS
  const [stepIdx, setStepIdx] = useState(0)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [language, setLanguage] = useState(locale)
  const requestedLanguage = useRef(language)
  const saving = useRef(false)
  const step = steps[stepIdx]
  const box = useRef<HTMLDivElement>(null)
  const touring = step === 'guide'
  useModalFocus(box, !touring)
  const finishTour = useCallback(() => setStepIdx(STEPS.indexOf('approval')), [])

  async function pickLanguage(l: Locale) {
    setLanguage(l)
    setError('')
    requestedLanguage.current = l
    try {
      // The first visit may need to fetch a dictionary. Only preview the last choice.
      await loadLocale(l)
      if (requestedLanguage.current !== l) return
      await setLocale(l)
      if (requestedLanguage.current !== l) return
    } catch (e) {
      if (requestedLanguage.current === l) {
        setLanguage(locale())
        setError(e instanceof Error ? e.message : String(e))
      }
      return
    }
  }

  async function save(patch: OnboardingPatch): Promise<OnboardingState | null> {
    if (saving.current) return null
    saving.current = true
    setBusy(true)
    setError('')
    try {
      return await api.postOnboarding(patch)
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
      return null
    } finally {
      saving.current = false
      setBusy(false)
    }
  }

  async function next() {
    // Persist the preselected language even when the user clicks Next without
    // touching a radio. The first-run gate treats an empty locale as unset.
    if (step === 'language' && !(await save({ locale: language }))) return
    if (step === 'terms') {
      const state = await save({ terms_accepted: true })
      if (!state) return
      if (props.termsOnly) { props.onDone(state); return }
    }
    if (stepIdx < steps.length - 1) {
      setStepIdx(stepIdx + 1)

    }
  }

  async function finish() {
    const state = await save({ onboarded: true })
    if (state) props.onDone(state)
  }

  // The tour handles Escape itself so closing it continues to approvals.
  useEffect(() => {
    if (touring) return
    const on = (e: KeyboardEvent) => {
      if (e.key === 'Escape' && !saving.current) props.onClose()
    }
    addEventListener('keydown', on)
    return () => removeEventListener('keydown', on)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [touring])

  if (touring) {
    return <GuideTour onFinish={finishTour} onPhase={props.onPhase} />
  }

  return (
    <div
      class="modal-backdrop"
      onClick={(e) => {
        if (e.target === e.currentTarget && !saving.current) props.onClose()
      }}
    >
      <div ref={box} class="modal onboarding-modal wizard"
        role="dialog" aria-modal="true" aria-label={t('onboarding.title')}>
        <div class="wizard-progress" aria-hidden="true">
          {steps.map((s, i) => (
            <span
              key={s}
              class={`wizard-dot${i < stepIdx ? ' done' : ''}${i === stepIdx ? ' on' : ''}`}
            />
          ))}
          <span class="wizard-step-label">{t(`onboarding.step.${step}`)}</span>
        </div>

        {step === 'language' && <LanguageStep value={language} onPick={pickLanguage} disabled={busy} />}
        {step === 'terms' && <TermsStep />}
        {step === 'approval' && (
          <ApprovalStep
            initial={props.state.approval_mode}
            onPick={async (mode) => {
              if (await save({ approval_mode: mode })) setStepIdx(stepIdx + 1)
            }}
            busy={busy}
          />
        )}
        {step === 'model' && <ModelStep onSaved={finish} />}

        {error && <p class="gate-error">{error}</p>}

        {step !== 'approval' && step !== 'model' && (
          <div class="modal-actions">
            <button type="button" class="btn" disabled={busy} onClick={props.onClose}>
              {t('onboarding.skip')}
            </button>
            <button
              type="button"
              class="btn primary"
              disabled={busy || (step === 'language' && language !== locale())}
              onClick={next}
            >
              {t(step === 'terms' ? 'onboarding.agree' : 'onboarding.next')}
            </button>
          </div>
        )}
      </div>
    </div>
  )
}

// ---- Step 1: language ----

function LanguageStep({ value, onPick, disabled }: {
  value: Locale
  onPick(l: Locale): void
  disabled: boolean
}) {
  return (
    <>
      <h2 class="modal-title wizard-language-title" id="wizard-language-title">
        {t('onboarding.langTitle')}
      </h2>
      <div class="wizard-langs" role="radiogroup" aria-labelledby="wizard-language-title">
        {locales.map((l) => (
          <label key={l} class="wizard-lang" lang={l}>
            <input
              class="wizard-lang-radio"
              type="radio"
              name="onboarding-language"
              value={l}
              checked={value === l}
              tabIndex={value === l ? 0 : -1}
              disabled={disabled}
              onChange={() => onPick(l)}
            />
            <span>{localeNames[l]}</span>
          </label>
        ))}
      </div>
    </>
  )
}

// ---- Step 2: terms ----

function TermsStep() {
  return (
    <>
      <h2 class="modal-title">{t('onboarding.termsTitle')}</h2>
      <TermsContent />
    </>
  )
}

// ---- Step 3: approval mode ----

function ApprovalStep(props: {
  initial: 'always' | 'on-request' | 'never'
  busy: boolean
  onPick(mode: 'always' | 'on-request' | 'never'): void
}) {
  const [mode, setMode] = useState(props.initial)
  return (
    <>
      <h2 class="modal-title">{t('onboarding.approvalTitle')}</h2>
      <p class="modal-msg">{t('onboarding.approvalSub')}</p>
      <div class="wizard-approvals" role="radiogroup">
        {(['always', 'on-request', 'never'] as const).map((m) => (
          <button
            key={m}
            type="button"
            role="radio"
            aria-checked={mode === m}
            class={`wizard-approval${mode === m ? ' on' : ''}`}
            onClick={() => setMode(m)}
          >
            <span class="wizard-approval-name">{t(`onboarding.approval.${m}`)}</span>
            <span class="wizard-approval-desc">{t(`onboarding.approval.${m}Desc`)}</span>
          </button>
        ))}
      </div>
      <div class="modal-actions">
        <span class="u-flex-1" />
        <button
          type="button"
          class="btn primary"
          disabled={props.busy}
          onClick={() => props.onPick(mode)}
        >
          {t('onboarding.next')}
        </button>
      </div>
    </>
  )
}

// ---- Step 4: model ----

type ApiType = 'anthropic' | 'openai' | 'responses' | 'google'

const EXAMPLES: Record<ApiType, { base: string; model: string }> = {
  anthropic: { base: 'https://api.anthropic.com', model: 'claude-sonnet-5-5' },
  openai: { base: 'https://api.openai.com/v1', model: 'gpt-6.1-sol' },
  responses: { base: 'https://api.openai.com/v1', model: 'gpt-6.1-sol' },
  google: { base: 'https://generativelanguage.googleapis.com/v1beta', model: 'gemini-4-argon' },
}

/** The model step mirrors the models page's add wizard: a catalogue card
 *  grid first (endpoints, default models, no-auth flags all prefilled),
 *  then a form that only asks for what the pick still needs — a key for
 *  hosted providers, nothing but a model id for Ollama-style ones, and the
 *  full wire shape for "custom". Saving goes through POST /api/models,
 *  which activates the first entry automatically — exactly the zero-config
 *  path this wizard exists for. */
function ModelStep(props: { onSaved(): void }) {
  const [providers, setProviders] = useState<ProviderInfo[] | null>(null)
  const [provider, setProvider] = useState<ProviderInfo | null>(null)
  const [model, setModel] = useState('')
  const [apiKey, setApiKey] = useState('')
  const [apiType, setApiType] = useState<ApiType>('openai')
  const [baseUrl, setBaseUrl] = useState('')
  // Consent to reach a plain-http endpoint; the network guard refuses the
  // endpoint without it.
  const [allowPlaintext, setAllowPlaintext] = useState(false)
  const plaintextEndpoint = needsPlaintextConsent(baseUrl)
  const [maxTokens, setMaxTokens] = useState(0)
  const [remoteModels, setRemoteModels] = useState<ModelRoster | null>(null)
  const [busy, setBusy] = useState('')
  const [error, setError] = useState('')
  const [testResult, setTestResult] = useState<{
    ok: boolean
    reply?: string
    error?: string
  } | null>(null)

  useEffect(() => {
    api
      .models()
      .then((r) => setProviders(r.providers))
      .catch(() => setProviders([])) // catalogue unreachable → custom form still works
  }, [])

  const regions = useMemo(() => {
    const groups: Record<string, ProviderInfo[]> = { global: [], cn: [] }
    for (const p of providers ?? []) {
      ;(groups[p.region] ?? groups.global)!.push(p)
    }
    return groups
  }, [providers])

  function pick(p: ProviderInfo) {
    setProvider(p)
    setModel(p.default_model)
    setBaseUrl(p.id === 'custom' ? '' : p.base_url)
    if (
      p.api_type === 'anthropic' ||
      p.api_type === 'openai' ||
      p.api_type === 'responses' ||
      p.api_type === 'google'
    ) {
      setApiType(p.api_type)
    }
    setRemoteModels(null)
    setTestResult(null)
    setError('')
  }

  function refPayload() {
    if (!provider) return {}
    return provider.id === 'custom'
      ? {
          provider: 'custom',
          api_type: apiType,
          base_url: baseUrl.trim(),
          model: model.trim(),
          api_key: apiKey.trim() || undefined,
          allow_plaintext: plaintextEndpoint ? allowPlaintext : undefined,
        }
      : {
          provider: provider.id,
          model: model.trim() || undefined,
          api_key: apiKey.trim() || undefined,
        }
  }

  const needsKey = provider !== null && !provider.no_auth && !provider.key_saved
  const ready =
    provider !== null &&
    (provider.id === 'custom'
      ? baseUrl.trim() !== '' && model.trim() !== ''
      : model.trim() !== '' || provider.default_model !== '') &&
    (!needsKey || apiKey.trim() !== '')

  async function test() {
    if (busy !== '' || !ready) return
    setBusy('test')
    setTestResult(null)
    try {
      const r = await api.testModelRef(refPayload())
      setTestResult(r.ok ? { ok: true, reply: r.reply } : { ok: false, error: r.error })
    } catch (e: unknown) {
      setTestResult({ ok: false, error: e instanceof Error ? e.message : String(e) })
    } finally {
      setBusy('')
    }
  }

  async function fetchRemote() {
    if (!provider || busy !== '') return
    setBusy('fetch')
    setError('')
    try {
      const r = await api.fetchModels(refPayload())
      if (r.ok && r.models) setRemoteModels({ ids: r.models, owners: r.owners ?? {} })
      else setError(r.error ?? t('models.fetchFail'))
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy('')
    }
  }

  async function save(e: Event) {
    e.preventDefault()
    if (!ready || busy !== '') return
    setBusy('save')
    setError('')
    try {
      await api.addModel({
        provider: provider!.id,
        model: model.trim() || undefined,
        api_key: apiKey.trim() || undefined,
        ...(provider!.id === 'custom'
          ? {
              api_type: apiType,
              base_url: baseUrl.trim(),
              max_tokens: maxTokens || undefined,
              allow_plaintext: plaintextEndpoint ? allowPlaintext : undefined,
            }
          : {}),
      })
      notifyModelSaved()
      toast(t('onboarding.saved'), 'success')
      props.onSaved()
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err))
      setBusy('')
    }
  }

  if (!providers) return <p class="hint">{t('common.loading')}</p>

  // Phase 1: the catalogue grid — same card markup as the models page.
  if (!provider) {
    return (
      <>
        <h2 class="modal-title">{t('onboarding.modelTitle')}</h2>
        <p class="modal-msg">{t('onboarding.subtitle')}</p>
        <p class="hint">{t('models.pickProvider')}</p>
        <div class="wizard-scroll">
          {(['global', 'cn'] as const).map(
            (r) =>
              regions[r]!.length > 0 && (
                <div key={r}>
                  <div class="section-title wizard-region">{t(`models.region.${r}`)}</div>
                  <div class="provider-grid">
                    {regions[r]!.map((p) => (
                      <ProviderCard key={p.id} provider={p} onPick={() => pick(p)} />
                    ))}
                  </div>
                </div>
              ),
          )}
        </div>
        <div class="modal-actions">
          <button type="button" class="btn" onClick={props.onSaved}>
            {t('onboarding.skip')}
          </button>
        </div>
      </>
    )
  }

  // Phase 2: only the fields the pick still needs.
  return (
    <form onSubmit={save}>
      <h2 class="modal-title">{t('onboarding.modelTitle')}</h2>
      <div class="wizard-picked">
        <button class="btn small ghost" type="button" onClick={() => setProvider(null)}>
          ← {provider.label}
        </button>
      </div>

      {provider.id === 'custom' && (
        <>
          <div class="field-group">
            <label>{t('settings.apiType')}</label>
            <div class="segmented" role="radiogroup">
              {(['openai', 'anthropic', 'responses', 'google'] as const).map((v) => (
                <button
                  key={v}
                  type="button"
                  role="radio"
                  aria-checked={apiType === v}
                  class={`seg${apiType === v ? ' on' : ''}`}
                  onClick={() => {
                    setApiType(v)
                    setTestResult(null)
                  }}
                >
                  {v === 'anthropic'
                    ? t('settings.anthropic')
                    : v === 'openai'
                      ? t('settings.openai')
                      : v === 'responses'
                        ? t('settings.responses')
                        : t('settings.google')}
                </button>
              ))}
            </div>
          </div>
          <div class="field-group">
            <label for="onboarding-base-url">{t('settings.baseURL')}</label>
            <input
              id="onboarding-base-url"
              class="input mono"
              type="url"
              required
              placeholder={EXAMPLES[apiType].base}
              value={baseUrl}
              onInput={(e) => {
                setBaseUrl((e.target as HTMLInputElement).value)
                setTestResult(null)
              }}
            />
            <p class="hint">{t('settings.baseURLHelp')}</p>
            {plaintextEndpoint && (
              <>
                <label class="checkbox-label">
                  <input
                    type="checkbox"
                    checked={allowPlaintext}
                    onChange={(e) => {
                      setAllowPlaintext((e.target as HTMLInputElement).checked)
                      setTestResult(null)
                    }}
                  />{' '}
                  {t('settings.allowPlaintext')}
                </label>
                <p class="hint">{t('settings.allowPlaintextHelp')}</p>
              </>
            )}
          </div>
        </>
      )}

      <div class="field-group">
        <label for="onboarding-model">{t('settings.model')}</label>
        <div class="field-row">
          <ModelPicker
            id="onboarding-model"
            value={model}
            onValue={(v) => {
              setModel(v)
              setTestResult(null)
            }}
            options={remoteModels?.ids ?? []}
            owners={remoteModels?.owners}
            placeholder={provider.default_model || EXAMPLES[apiType].model}
            required={provider.id === 'custom'}
          />
          <button type="button" class="btn" disabled={busy !== ''} onClick={fetchRemote}>
            {busy === 'fetch' ? '…' : t('models.fetch')}
          </button>
        </div>
        {remoteModels && <p class="hint">{t('models.fetched', { n: remoteModels.ids.length })}</p>}
      </div>

      {!provider.no_auth && (
        <div class="field-group">
          <label for="onboarding-api-key">{t('settings.apiKey')}</label>
          <input
            id="onboarding-api-key"
            class="input mono"
            type="password"
            autocomplete="off"
            placeholder={provider.key_saved ? t('models.keyReuseHint') : 'sk-…'}
            value={apiKey}
            onInput={(e) => setApiKey((e.target as HTMLInputElement).value)}
          />
          {provider.key_saved && <p class="hint">{t('models.keyReuseHelp')}</p>}
        </div>
      )}

      {provider.id === 'custom' && (
        <div class="field-group">
          <label for="onboarding-max-tokens">{t('settings.maxTokens')}</label>
          <input
            id="onboarding-max-tokens"
            class="input"
            type="number"
            min={0}
            value={maxTokens || ''}
            onInput={(e) => setMaxTokens(Number((e.target as HTMLInputElement).value) || 0)}
          />
        </div>
      )}

      {testResult && (
        <p class={`test-result ${testResult.ok ? 'ok' : 'bad'}`}>
          {testResult.ok
            ? t('settings.testOk', { reply: (testResult.reply || '').slice(0, 80) })
            : `${t('settings.testFail')} ${testResult.error ?? ''}`}
        </p>
      )}
      {error && <p class="gate-error">{error}</p>}

      <div class="modal-actions">
        <button type="button" class="btn" onClick={props.onSaved}>
          {t('onboarding.skip')}
        </button>
        <button type="button" class="btn" disabled={busy !== '' || !ready} onClick={test}>
          {busy === 'test' ? t('settings.testing') : t('settings.test')}
        </button>
        <button type="submit" class="btn primary" disabled={!ready || busy === 'save'}>
          {busy === 'save' ? t('common.save') + '…' : t('onboarding.finish')}
        </button>
      </div>
    </form>
  )
}
