// SPDX-License-Identifier: AGPL-3.0-or-later

import assert from 'node:assert/strict'
import { test } from 'node:test'
import type { ModelEntry, ModelsResponse, ProviderInfo } from '../api/client.ts'
import { setLocale } from '../i18n/index.ts'
import {
  connHost,
  connIdOf,
  connImported,
  connName,
  connRefs,
  copyAlias,
  derivedConnName,
  groupConnections,
  hostOf,
  isLoopbackURL,
  keyOf,
  lenderOf,
  refOf,
} from './model-connections.ts'

/** A wire entry with only the fields these tests care about; the rest of the
 *  payload is the panel's and is not read here. */
function entry(over: Partial<ModelEntry>): ModelEntry {
  return {
    alias: 'relay',
    model: 'm-1',
    base_url: 'http://127.0.0.1:3065/v1',
    api_type: 'openai',
    key_set: true,
    active: false,
    ...over,
  } as ModelEntry
}

function response(models: ModelEntry[], active?: ModelEntry): ModelsResponse {
  return { models, active: active ?? models[0], providers: [] } as ModelsResponse
}

test('two accounts of one vendor are different connections', () => {
  // Same provider, same endpoint, same last four characters — only the key
  // differs. Grouping them together made "extend this connection" borrow the
  // other account's key.
  const a = entry({ key_fingerprint: 'aaaa1111', key_hint: '1234' })
  const b = entry({ key_fingerprint: 'bbbb2222', key_hint: '1234' })
  assert.notEqual(connIdOf(a), connIdOf(b))
})

test('an older panel without a digest still groups by key_hint', () => {
  const a = entry({ key_hint: '1234' })
  assert.ok(connIdOf(a).includes('1234'))
})

test('grouping keeps the aliases of one connection together', () => {
  const conns = groupConnections(
    response([
      entry({ alias: 'relay', model: 'm-1' }),
      entry({ alias: 'relay-cheap', model: 'm-2' }),
    ]),
  )
  assert.equal(conns.length, 1)
  assert.equal(conns[0]!.members.length, 2)
  assert.equal(conns[0]!.baseURL, 'http://127.0.0.1:3065/v1')
})

test('the live model configured outside the registry gets its own row', () => {
  const live = entry({ alias: 'live', model: 'x', base_url: 'https://elsewhere.example/v1' })
  const conns = groupConnections(response([entry({ alias: 'relay' })], live))
  assert.equal(conns.length, 2)
  assert.ok(conns.some((c) => c.members.some((m) => !m.registry)))
})

test('a registry entry that is also the live model is not duplicated', () => {
  const live = entry({ alias: 'relay', active: true })
  const conns = groupConnections(response([entry({ alias: 'relay', active: true })], live))
  assert.equal(conns.length, 1)
  assert.equal(conns[0]!.members.length, 1)
})

test('the active connection sorts first', () => {
  const idle = entry({ alias: 'zzz', base_url: 'https://b.example/v1' })
  const active = entry({ alias: 'aaa', base_url: 'https://a.example/v1', active: true })
  const conns = groupConnections(response([idle, active], active))
  assert.equal(conns[0]!.members[0]!.entry.alias, 'aaa')
})

test('copyAlias walks past the suffixes already taken', () => {
  assert.equal(copyAlias([], 'relay'), 'relay-2')
  assert.equal(copyAlias(['relay-2'], 'relay'), 'relay-3')
  assert.equal(copyAlias(['relay-2', 'relay-3'], 'relay'), 'relay-4')
})

test('keyOf distinguishes rows that differ only by model', () => {
  assert.notEqual(keyOf(entry({ model: 'a' })), keyOf(entry({ model: 'b' })))
})

test('keyOf distinguishes two accounts that share every visible field', () => {
  // The alias, the model id and the endpoint are what two accounts of one
  // vendor share; the key digest is the one thing that tells them apart, so it
  // is part of the React key and of the per-row busy flag.
  const work = entry({ key_fingerprint: 'aaaa1111' })
  const home = entry({ key_fingerprint: 'bbbb2222' })
  assert.notEqual(keyOf(work), keyOf(home))
})

test('refOf carries the key digest so one account can be addressed', () => {
  const ref = refOf(entry({ key_fingerprint: 'aaaa1111' }))
  assert.equal(ref.key, 'aaaa1111')
  assert.equal(ref.model, 'm-1')
  // A row with no key must not send an empty narrowing parameter: the server
  // would treat "key=" as no key at all, which is right, but the query string
  // should stay clean.
  assert.equal(refOf(entry({ key_fingerprint: undefined })).key, undefined)
})

test('hostOf prints an unparsable address as typed', () => {
  assert.equal(hostOf('http://127.0.0.1:3065/v1'), '127.0.0.1:3065')
  assert.equal(hostOf('not a url'), 'not a url')
  assert.equal(hostOf('  '), '')
})

test('connRefs addresses every row of the card and nothing else', () => {
  // Clearing a card sends the rows it is showing. The neighbour on another
  // endpoint is one card away and must not be in the list: the server deletes
  // what it is told, so an over-broad address takes a working account with it.
  const conns = groupConnections(
    response([
      entry({ alias: 'relay', model: 'm-1', base_url: 'https://a.example/v1', key_fingerprint: 'aaaa' }),
      entry({ alias: 'relay-2', model: 'm-2', base_url: 'https://a.example/v1', key_fingerprint: 'aaaa' }),
      entry({ alias: 'spare', model: 's', base_url: 'https://b.example/v1', key_fingerprint: 'bbbb' }),
    ]),
  )
  const refs = connRefs(conns.find((c) => c.baseURL === 'https://a.example/v1')!)
  assert.deepEqual(refs, [
    { alias: 'relay', model: 'm-1', base_url: 'https://a.example/v1', key: 'aaaa' },
    { alias: 'relay-2', model: 'm-2', base_url: 'https://a.example/v1', key: 'aaaa' },
  ])
})

test('connRefs includes the live row that has no registry entry', () => {
  // That row is on screen — the page offers it edit and delete — so a card
  // clear that skipped it would leave the card standing with one row in it.
  const live = entry({ alias: 'live', model: 'x', base_url: 'https://elsewhere.example/v1' })
  const conns = groupConnections(response([entry({ alias: 'relay' })], live))
  const refs = connRefs(conns.find((c) => c.baseURL === 'https://elsewhere.example/v1')!)
  assert.deepEqual(
    refs.map((r) => r.alias),
    ['live'],
  )
})

/** A catalogue row as the panel sends it. */
function provider(id: string, label: string): ProviderInfo {
  return { id, label, api_type: 'openai', base_url: '', default_model: '', no_auth: false, key_saved: false, region: 'cn' }
}

test('an imported relay is named after the tool, not its loopback address', () => {
  setLocale('en')
  // The person never typed 127.0.0.1:7896 — their Claude Code settings did.
  // The card owes them the tool's name, and it owes them the port nowhere.
  const conns = groupConnections(
    response([
      entry({ source: 'claude', provider: 'custom', base_url: 'http://127.0.0.1:7896', api_type: 'anthropic' }),
    ]),
  )
  const c = conns[0]!
  assert.equal(connName(c, []), 'Claude Code')
  assert.equal(connHost(c), 'Local endpoint')
  assert.ok(connImported(c))
})

test('a known vendor keeps its brand even when imported', () => {
  setLocale('en')
  // "ChatGPT (OpenAI)" is the best a card can say; the badge carries the rest.
  const conns = groupConnections(
    response([
      entry({
        source: 'codex',
        provider: 'codex',
        provider_label: 'ChatGPT (OpenAI)',
        base_url: 'https://chatgpt.com/backend-api/codex/responses',
        api_type: 'responses',
      }),
    ]),
  )
  const c = conns[0]!
  assert.equal(connName(c, [provider('codex', 'ChatGPT (OpenAI)')]), 'ChatGPT (OpenAI)')
  assert.equal(connHost(c), 'chatgpt.com')
  assert.ok(connImported(c))
})

test('a hand-written relay keeps the short label and hides its port', () => {
  setLocale('en')
  const conns = groupConnections(response([entry({ provider: 'custom', base_url: 'http://127.0.0.1:3065/v1' })]))
  const c = conns[0]!
  assert.equal(connName(c, [provider('custom', '自定义 / 中转站 (custom base_url)')]), 'Custom endpoint')
  assert.equal(connHost(c), 'Local endpoint')
  assert.equal(connImported(c), false)
})

test('a name the user typed outranks the derived one', () => {
  setLocale('en')
  // Two relays on loopback ports are the same card without a name: same
  // "Custom endpoint" heading, same "Local endpoint" line. The stored name is
  // the only thing on the face that tells them apart.
  const conns = groupConnections(
    response([
      entry({ provider: 'custom', base_url: 'http://127.0.0.1:3065', conn_name: '公司中转' }),
    ]),
  )
  const c = conns[0]!
  assert.equal(c.customName, '公司中转')
  assert.equal(connName(c, [provider('custom', '自定义 / 中转站 (custom base_url)')]), '公司中转')
  // The derived heading is still what an empty field would produce — the edit
  // form paints it as the placeholder.
  assert.equal(derivedConnName(c, []), 'Custom endpoint')
})

test('a nameless row does not hide a name its siblings carry', () => {
  setLocale('en')
  // A copy made by an older console lands without the field; the group keeps
  // the name the other row of the same account already has.
  const conns = groupConnections(
    response([
      entry({ alias: 'a', model: 'm-1' }),
      entry({ alias: 'b', model: 'm-2', conn_name: '公司中转' }),
    ]),
  )
  assert.equal(conns.length, 1)
  assert.equal(conns[0]!.customName, '公司中转')
})

test('one imported row labels the whole account', () => {
  const conns = groupConnections(
    response([
      entry({ alias: 'a', model: 'm-1' }),
      entry({ alias: 'b', model: 'm-2', source: 'opencode' }),
    ]),
  )
  assert.equal(conns.length, 1)
  assert.equal(conns[0]!.source, 'opencode')
})

test('CC Switch connections keep the provider name and remain recognizable without one', () => {
  setLocale('en')
  const [connection] = groupConnections(response([
    entry({ provider: 'custom', base_url: 'https://relay.invalid/v1', source: 'ccswitch', conn_name: 'My relay' }),
  ]))
  assert.ok(connImported(connection!))
  assert.equal(connName(connection!, []), 'My relay')
  assert.equal(derivedConnName(connection!, []), 'CC Switch')
})

test('loopback detection covers localhost, v4 and v6', () => {
  assert.ok(isLoopbackURL('http://127.0.0.1:3065/v1'))
  assert.ok(isLoopbackURL('http://127.9.9.9:1/v1'))
  assert.ok(isLoopbackURL('http://localhost:1234/v1'))
  assert.ok(isLoopbackURL('http://[::1]:1234/v1'))
  assert.equal(isLoopbackURL('https://dashscope.aliyuncs.com/compatible-mode/v1'), false)
  assert.equal(isLoopbackURL('not a url'), false)
})

test('a new row borrows the key of the account it names, not the first alias match', () => {
  const a = entry({ alias: 'shared', model: 'm-a', key_fingerprint: 'aaaa1111' })
  const b = entry({ alias: 'shared', model: 'm-b', key_fingerprint: 'bbbb2222' })
  // The trash marks A; B is kept and lends. An alias-only borrow lets the
  // server resolve whichever row carries the name — in the report, A — so the
  // fingerprint is what pins the new row to B's credential.
  const lender = lenderOf(
    [
      { id: 1, alias: 'shared', dropped: true, src: a },
      { id: 2, alias: 'shared', src: b },
    ],
  )
  assert.deepEqual(lender, { alias: 'shared', key: 'bbbb2222' })
})

test('a dropped lender keeps its stored name; a renamed kept row lends its saved name', () => {
  const a = entry({ alias: 'old', key_fingerprint: 'aaaa1111' })
  const dropped = lenderOf([{ id: 1, alias: 'typed-in-trash', dropped: true, src: a }])
  assert.deepEqual(dropped, { alias: 'old', key: 'aaaa1111' })

  const kept = lenderOf([{ id: 2, alias: 'typed', src: a }], new Map([[2, 'old-2']]))
  assert.deepEqual(kept, { alias: 'old-2', key: 'aaaa1111' })
})

test('a connection with no keyed row has no lender', () => {
  assert.equal(lenderOf([{ id: 1, alias: 'x', src: entry({ key_set: false, key_fingerprint: undefined }) }]), undefined)
  assert.equal(lenderOf([{ id: 2, alias: 'y' }]), undefined)
})
