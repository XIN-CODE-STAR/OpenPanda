// SPDX-License-Identifier: AGPL-3.0-or-later

/** True when a model endpoint is plain http on a host that is not loopback —
 *  the one shape the network guard refuses without an explicit per-model
 *  opt-in, because the API key is what travels in the clear. Loopback needs no
 *  consent (a local dev model is the case the exemption exists for), and an
 *  https URL never does.
 *
 *  This mirrors security.NetworkGuard's rule; it decides only whether the form
 *  shows the consent checkbox, never whether the request is allowed — the
 *  backend still refuses an unconsented plaintext endpoint. */
export function needsPlaintextConsent(url: string): boolean {
  // A bracketed IPv6 literal has to be matched whole: its colons would
  // otherwise end the host at "[".
  const m = /^http:\/\/(\[[^\]]*\]|[^/:?#]+)/i.exec(url.trim())
  if (!m?.[1]) return false
  const host = m[1].toLowerCase().replace(/^\[|\]$/g, '')
  if (host === 'localhost' || host === '::1') return false
  return !/^127\./.test(host)
}
