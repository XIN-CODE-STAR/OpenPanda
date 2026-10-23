// SPDX-License-Identifier: AGPL-3.0-or-later

import { useMemo, useRef } from 'preact/hooks'
import { locale, t, type Locale } from '../i18n'
import { useLocaleRerender } from '../hooks'
import { Markdown } from '../md/render'
import { useModalFocus } from './modal-focus'

// The card shows the repository's own TERMS files rather than a second copy of
// the same prose: `?raw` folds each translation in at build time, so the text a
// reader consents to and the text under version control cannot drift apart. The
// five files map one-to-one onto the console's five locales.
import termsDe from '../../../../TERMS.de.md?raw'
import termsEn from '../../../../TERMS.md?raw'
import termsEs from '../../../../TERMS.es.md?raw'
import termsJa from '../../../../TERMS.ja.md?raw'
import termsZh from '../../../../TERMS.zh-CN.md?raw'
import { prepareTerms } from './terms-text'

const SOURCES: Record<Locale, string> = {
  en: termsEn,
  'zh-CN': termsZh,
  ja: termsJa,
  es: termsEs,
  de: termsDe,
}

/** The markdown the card renders, in the console's current language. */
export function termsMarkdown(): string {
  return prepareTerms(SOURCES[locale()])
}

/** The license & disclaimer body shared by the onboarding wizard and the
 *  Settings → General "read terms" entry point — all seventeen sections of
 *  TERMS.md, in the console's language. */
export function TermsContent() {
  useLocaleRerender()
  const text = useMemo(() => termsMarkdown(), [locale()])
  return (
    <div class="wizard-terms">
      <Markdown text={text} />
    </div>
  )
}

/** A standalone modal for re-reading the terms after onboarding — same
 *  backdrop/focus-trap contract as the confirm dialog. */
export function TermsModal({ onClose }: { onClose(): void }) {
  const box = useRef<HTMLDivElement>(null)
  useModalFocus(box, true)

  return (
    <div
      class="modal-backdrop"
      onClick={(e) => {
        if (e.target === e.currentTarget) onClose()
      }}
    >
      <div
        ref={box}
        class="modal terms-modal"
        role="dialog"
        aria-modal="true"
        aria-label={t('onboarding.termsTitle')}
        onKeyDown={(e) => {
          if (e.key === 'Escape') {
            e.stopPropagation()
            onClose()
          }
        }}
      >
        <h2 class="modal-title">{t('onboarding.termsTitle')}</h2>
        <TermsContent />
        <div class="modal-actions">
          <button class="btn primary" onClick={onClose}>
            {t('common.close')}
          </button>
        </div>
      </div>
    </div>
  )
}
