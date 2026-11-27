// SPDX-License-Identifier: AGPL-3.0-or-later

import { useRef, useState } from 'preact/hooks'
import { api, type DiscoverSource, type ModelImportResult } from '../api/client'
import { t } from '../i18n'

export function useModelImport(
  onDone: (result: ModelImportResult) => void,
  onClose: () => void,
) {
  const submitting = useRef(false)
  const [busy, setBusy] = useState(false)
  const [failures, setFailures] = useState<string[]>([])

  async function submit(sources: DiscoverSource[], picked: Set<string>, complete: (ids: Set<string>) => void) {
    if (submitting.current) return
    const selected = sources.flatMap((source) => source.candidates)
      .filter((candidate) => candidate.importable && !candidate.existing && picked.has(candidate.id))
    if (!selected.length) return
    submitting.current = true
    setBusy(true)
    setFailures([])
    try {
      const result = await api.importModels(selected.map((candidate) => ({
        candidate_id: candidate.id,
        provider: candidate.provider,
        model: candidate.model,
        base_url: candidate.base_url || undefined,
        api_type: candidate.api_type || undefined,
        key_ref: candidate.key_ref || undefined,
        no_auth: candidate.no_auth,
        auth: candidate.auth || undefined,
        context_window: candidate.context_window,
      })))
      const skipped = result.skipped ?? []
      complete(new Set(result.completed_ids ?? (skipped.length === 0 ? selected.map((c) => c.id) : [])))
      onDone(result)
      if (skipped.length) setFailures(skipped)
      else onClose()
    } catch (error: unknown) {
      setFailures([error instanceof Error ? error.message : String(error)])
    } finally {
      submitting.current = false
      setBusy(false)
    }
  }

  return { busy, failures, submit, clearFailures: () => setFailures([]), close: () => {
    if (!submitting.current) onClose()
  } }
}

export function markImported(sources: DiscoverSource[] | null, ids: Set<string>): DiscoverSource[] | null {
  return sources?.map((source) => ({
    ...source,
    candidates: source.candidates.map((candidate) => ids.has(candidate.id) ? { ...candidate, existing: true } : candidate),
  })) ?? null
}

export function ImportFailures({ failures }: { failures: string[] }) {
  if (!failures.length) return null
  return (
    <div class="gate-error discover-import-feedback" role="alert">
      <strong>{t('models.discover.importIncomplete')}</strong>
      <ul>
        {failures.map((reason, index) => <li key={index}>{reason}</li>)}
      </ul>
    </div>
  )
}
