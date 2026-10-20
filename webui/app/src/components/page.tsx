// SPDX-License-Identifier: AGPL-3.0-or-later

import { t } from '../i18n'

// Shared page skeleton (P1: "设计语言统一"). Every view renders the same
// header shape, the same friendly error with a retry, and the same loading
// hint — before this, each view improvised: some dropped the page title on
// error, some showed raw `common.error (message)` paragraphs, and loading
// states differed per page.

/** Standard page header: title + one-line description. */
export function PageHeader({ title, sub }: { title: string; sub: string }) {
  return (
    <>
      <h1 class="page-title">{title}</h1>
      <p class="page-sub">{sub}</p>
    </>
  )
}

/** Friendly full-page error: keeps the page header so the user knows where
 *  they are, explains that loading failed (not the raw fetch error), and
 *  offers a retry. The technical detail stays for diagnosis. `head={false}`
 *  drops the header for boards hosted inside a merged page (队列·任务·项目), which
 *  paints the heading itself. */
export function ErrorState({
  head = true,
  title,
  sub,
  error,
  onRetry,
}: {
  head?: boolean
  title: string
  sub: string
  error: string
  onRetry(): void
}) {
  return (
    <section>
      {head && <PageHeader title={title} sub={sub} />}
      <div class="card error-state">
        <span class="error-state-title">{t('common.loadFail')}</span>
        <span class="dim error-state-detail">{error}</span>
        <div>
          <button class="btn" onClick={onRetry}>
            {t('common.retry')}
          </button>
        </div>
      </div>
    </section>
  )
}

/** Full-page loading under the page header. `head={false}` drops the header
 *  for pages that paint no header once loaded — otherwise the title flashes
 *  in and then vanishes. */
export function LoadingState({
  head = true,
  title,
  sub,
}: {
  head?: boolean
  title: string
  sub: string
}) {
  return (
    <section>
      {head && <PageHeader title={title} sub={sub} />}
      <p class="dim">
        <span class="spinner spinner-inline" aria-hidden="true" />
        {t('common.loading')}
      </p>
    </section>
  )
}
