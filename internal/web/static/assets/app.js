// gero web UI. Plain DOM, no framework, no build step.
//
// Everything an agent says is untrusted (a repository or a web page can put
// words in its mouth), so agent text only ever reaches the page through
// text nodes: h() below never parses HTML.

const root = document.getElementById('app')

function h(tag, props, ...children) {
  const el = document.createElement(tag)
  for (const [k, v] of Object.entries(props || {})) {
    if (v == null || v === false) continue
    if (k === 'class') el.className = v
    else if (k.startsWith('on')) el.addEventListener(k.slice(2), v)
    else if (k === 'value' || k === 'checked' || k === 'disabled' || k === 'open') el[k] = v
    else el.setAttribute(k, v === true ? '' : v)
  }
  for (const c of children.flat(Infinity)) {
    if (c == null || c === false) continue
    el.append(c instanceof Node ? c : document.createTextNode(String(c)))
  }
  return el
}

// ---- API

class Unauthorized extends Error {}

async function api(method, path, body) {
  const res = await fetch(path, {
    method,
    credentials: 'same-origin',
    headers: { 'Content-Type': 'application/json', 'X-Gero-Request': '1' },
    body: body === undefined ? undefined : JSON.stringify(body),
  })
  if (res.status === 401) throw new Unauthorized()
  const data = await res.json().catch(() => ({}))
  if (!res.ok) throw new Error(data.error || res.statusText)
  return data
}

// ---- State

const S = {
  authed: true,
  agents: [],
  isolations: [],
  sel: null, // agent id
  view: 'agent', // agent | new | isolations
  showArchived: false,
  drawer: false,
  convos: new Map(),
  drafts: new Map(),
  es: null,
  esFor: null,
  settingsOpen: false,
  toast: null,
  // The credential proxies: services and their envs, and what each
  // isolation exposes. openForm names the one inline form that is open;
  // while it is, polling does not redraw the page under the operator.
  services: [],
  envs: [],
  exposures: {},
  openForm: null,
  login: null,
  checkOut: null,
  verifyOut: null, // the open verify report
  verifying: null, // the isolation being measured
}

const coarse = matchMedia('(pointer: coarse)').matches
const agentById = (id) => S.agents.find((a) => a.id === id)
const label = (a) => (a.kind === 'codex' ? 'Codex' : 'Claude')
const efforts = (kind) =>
  kind === 'codex' ? ['', 'low', 'medium', 'high', 'xhigh'] : ['', 'low', 'medium', 'high', 'xhigh', 'max']

function toast(msg, err) {
  S.toast = { msg, err }
  renderToast()
  clearTimeout(toast.t)
  toast.t = setTimeout(() => { S.toast = null; renderToast() }, 4000)
}

async function guarded(fn) {
  try {
    return await fn()
  } catch (e) {
    if (e instanceof Unauthorized) { S.authed = false; render(); return }
    console.error(e)
    toast(e.message, true)
  }
}

// ---- Conversation: events folded into what a person reads (mirrors the TUI)

function convo(id) {
  if (!S.convos.has(id)) S.convos.set(id, { lastID: 0, entries: [], byItem: new Map() })
  return S.convos.get(id)
}

function apply(c, e) {
  if (e.id) {
    if (e.id <= c.lastID) return
    c.lastID = e.id
  }
  const add = (en) => { c.entries.push(en); if (en.item) c.byItem.set(en.item, en); return en }
  switch (e.kind) {
    case 'user': add({ kind: 'user', text: e.text || '' }); break
    case 'delta': {
      let en = c.byItem.get(e.item)
      if (!en || en.kind !== 'assistant' || !en.live) en = add({ kind: 'assistant', item: e.item, live: true, text: '' })
      en.text += e.text || ''
      break
    }
    case 'assistant': {
      const en = c.byItem.get(e.item)
      if (en && en.kind === 'assistant' && en.live) { en.text = e.text; en.live = false }
      else add({ kind: 'assistant', item: e.item, text: e.text || '' })
      break
    }
    case 'reasoning': add({ kind: 'reasoning', text: e.text || '' }); break
    case 'tool_start':
      add({ kind: 'tool', item: e.item, name: e.tool?.name || '', summary: e.tool?.summary || '', output: '', state: 'running' })
      break
    case 'tool_output': {
      const en = c.byItem.get(e.item)
      if (en && en.kind === 'tool' && en.output.length < 16384) en.output += e.text || ''
      break
    }
    case 'tool_end': {
      let en = c.byItem.get(e.item)
      if (!en || en.kind !== 'tool') en = add({ kind: 'tool', item: e.item, name: '', summary: '', output: '', state: 'running' })
      if (e.tool) {
        if (e.tool.name) en.name = e.tool.name
        if (e.tool.summary) en.summary = e.tool.summary
        en.output = (e.tool.output || '').slice(0, 16384)
        en.state = e.tool.is_error ? 'failed' : 'ok'
      }
      break
    }
    // The approval's own kind (command, tool, ...) must not overwrite the
    // entry's: it is kept as akind.
    case 'approval': add({ ...e.approval, akind: e.approval.kind, kind: 'approval' }); break
    case 'approval_done':
      for (const en of c.entries) if (en.kind === 'approval' && en.id === e.approval.id) en.decision = e.approval.decision
      break
    case 'status':
      // Drivers report blocked after every approval and leave it only when
      // none is open: any other status means nothing is pending.
      if (e.status !== 'blocked') for (const en of c.entries) if (en.kind === 'approval' && !en.decision) en.decision = 'void'
      break
    case 'turn_end':
      if (e.text === 'interrupted') add({ kind: 'interrupted' })
      for (const en of c.entries) en.live = false
      break
    case 'error': add({ kind: 'error', text: e.text }); break
    case 'notice':
      if (!(e.text || '').startsWith('thread/')) add({ kind: 'notice', text: e.text })
      break
  }
}

const pending = (c) => c.entries.filter((en) => en.kind === 'approval' && !en.decision)
const decisionText = (d) => ({ accept: 'accepted', decline: 'declined', cancel: 'declined, turn stopped', void: 'the process ended' })[d] || d
const askText = (kind) => ({ command: 'run a command', file_change: 'change files', tool: 'use a tool' })[kind] || kind

// richText renders markdown-ish agent text: fenced code, `inline code`,
// **bold**, paragraphs. Text nodes only.
function richText(text) {
  const frag = document.createDocumentFragment()
  text.split('```').forEach((part, i) => {
    if (i % 2 === 1) {
      const nl = part.indexOf('\n')
      const body = nl >= 0 && /^[\w+.-]*$/.test(part.slice(0, nl)) ? part.slice(nl + 1) : part
      frag.append(h('pre', { class: 'code' }, h('code', null, body.replace(/\n$/, ''))))
      return
    }
    for (const para of part.split(/\n{2,}/)) {
      if (!para.trim()) continue
      const p = h('p')
      para.split('\n').forEach((line, j) => { if (j) p.append(h('br')); p.append(...inline(line)) })
      frag.append(p)
    }
  })
  return frag
}

function inline(line) {
  const out = []
  const re = /(`[^`]+`|\*\*[^*]+\*\*)/g
  let last = 0
  let m
  while ((m = re.exec(line))) {
    if (m.index > last) out.push(line.slice(last, m.index))
    const t = m[0]
    out.push(t[0] === '`' ? h('code', null, t.slice(1, -1)) : h('strong', null, t.slice(2, -2)))
    last = m.index + t.length
  }
  if (last < line.length) out.push(line.slice(last))
  return out
}

function clip(s, n) {
  const lines = s.replace(/\n+$/, '').split('\n')
  return lines.length <= n ? lines.join('\n') : lines.slice(0, n).join('\n') + `\n… ${lines.length - n} more lines`
}

// ---- Event stream for the open agent

function follow(id) {
  if (S.esFor === id && S.es && S.es.readyState !== EventSource.CLOSED) return
  if (S.es) S.es.close()
  const c = convo(id)
  const es = new EventSource(`/v1/agents/${encodeURIComponent(id)}/events?follow=1&after=${c.lastID}`)
  es.onmessage = (m) => {
    const e = JSON.parse(m.data)
    apply(c, e)
    // Status rides the same stream; show it now rather than at the next poll.
    if (e.kind === 'status' && !e.ephemeral) {
      const a = agentById(id)
      if (a && a.state?.status !== e.status) {
        a.state = { ...a.state, status: e.status }
        renderSidebar()
        if (S.sel === id) renderTopbar()
      }
    }
    if (S.sel === id) scheduleConvo()
  }
  es.onerror = () => {
    if (es.readyState === EventSource.CLOSED && S.sel === id) setTimeout(() => S.sel === id && follow(id), 2000)
  }
  S.es = es
  S.esFor = id
}

// Coalesce a burst of events into one render. A timer rather than
// requestAnimationFrame: rAF never fires in a hidden tab, and a tab brought
// back should show what happened while it was away.
let convoQueued = false
function scheduleConvo() {
  if (convoQueued) return
  convoQueued = true
  setTimeout(() => { convoQueued = false; renderConvo(); renderApprovals() }, 30)
}

// ---- Polling

async function refreshAgents() {
  const agents = await api('GET', '/v1/agents')
  S.agents = agents
  if (!S.sel && S.view === 'agent') {
    const live = agents.filter((a) => !a.archived_at)
    if (live.length) select(live[live.length - 1].id, false)
  }
  // A deep link (#/a/<id>) is drawn before the list arrives; draw the agent
  // once it is known.
  if (S.view === 'agent' && S.sel && S.drawn !== S.sel && agentById(S.sel)) { render(); return }
  renderSidebar()
  if (S.view === 'agent') renderTopbar()
}

async function refreshIsolations() {
  S.isolations = await api('GET', '/v1/isolations')
  if (S.view === 'isolations') {
    await refreshExposures()
    if (!editing('isolations')) renderIsolations()
  }
  if (S.view === 'new') S.refillIsolations?.()
}

// editing is whether redrawing page id now would lose what the operator is
// typing: an inline form is open, or a field on it has the focus.
function editing(id) {
  if (S.openForm) return true
  const a = document.activeElement
  return !!(a && a.closest && a.closest('#' + id) && /^(INPUT|SELECT|TEXTAREA)$/.test(a.tagName))
}

async function refreshServices() {
  const [services, envs] = await Promise.all([api('GET', '/v1/services'), api('GET', '/v1/envs')])
  S.services = services || []
  S.envs = envs || []
  if (S.view === 'services' && !editing('services')) renderServices()
}

async function refreshExposures() {
  const out = {}
  await Promise.all(S.isolations.map(async (i) => { out[i.name] = await api('GET', `/v1/isolations/${encodeURIComponent(i.name)}/services`) }))
  S.exposures = out
  if (!S.services.length) {
    const [services, envs] = await Promise.all([api('GET', '/v1/services'), api('GET', '/v1/envs')])
    S.services = services || []
    S.envs = envs || []
  }
}

function startPolling() {
  let n = 0
  const tick = async () => {
    // The first load always happens; after that a hidden tab stops polling
    // and catches up the moment it is shown again.
    if ((n === 0 || !document.hidden) && S.authed) {
      await guarded(refreshAgents)
      // The isolations list runs `container inspect` per isolation; keep it
      // to every 10s unless that page is open.
      if (S.view !== 'agent' || n % 5 === 0) await guarded(refreshIsolations)
      if (S.view === 'services') await guarded(refreshServices)
    }
    n++
    setTimeout(tick, 2000)
  }
  tick()
  document.addEventListener('visibilitychange', () => {
    if (!document.hidden && S.authed) { guarded(refreshAgents); guarded(refreshIsolations) }
  })
}

// ---- Actions

// The address bar mirrors the view (#/a/<id>, #/new, #/isolations), so a
// page can be bookmarked and a phone's back gesture does the expected thing.
function setHash(hash) {
  if (location.hash !== hash) history.pushState(null, '', hash)
}

function select(id, closeDrawer = true) {
  if (S.sel && S.sel !== id) S.drafts.set(S.sel, document.querySelector('.composer textarea')?.value || '')
  S.sel = id
  S.view = 'agent'
  S.settingsOpen = false
  if (closeDrawer) S.drawer = false
  setHash('#/a/' + id)
  follow(id)
  render()
}

function show(view) {
  S.view = view
  S.drawer = false
  S.settingsOpen = false
  S.openForm = null
  setHash({ new: '#/new', isolations: '#/isolations', services: '#/services' }[view] || location.hash)
  render()
  if (view !== 'agent') guarded(refreshIsolations)
  if (view === 'services') guarded(refreshServices)
}

function route() {
  const m = location.hash.match(/^#\/a\/([a-z0-9-]+)$/)
  if (m) { S.sel = m[1]; S.view = 'agent'; follow(m[1]) }
  else if (location.hash === '#/new') S.view = 'new'
  else if (location.hash === '#/isolations') S.view = 'isolations'
  else if (location.hash === '#/services') { S.view = 'services'; guarded(refreshServices) }
  else S.view = 'agent'
  S.drawer = false
  S.settingsOpen = false
  render()
}
window.addEventListener('popstate', route)

async function send(text) {
  const id = S.sel
  await guarded(() => api('POST', `/v1/agents/${id}/messages`, { text }))
}

async function decide(approvalID, decision) {
  await guarded(() => api('POST', `/v1/agents/${S.sel}/approvals/${encodeURIComponent(approvalID)}`, { decision }))
}

// ---- Rendering

function render() {
  root.className = 'app' + (S.drawer ? ' drawer' : '')
  root.replaceChildren()
  if (!S.authed) {
    root.style.display = 'block'
    root.append(loginView())
    return
  }
  root.style.display = ''
  root.append(h('aside', { class: 'sidebar', id: 'sidebar' }), h('div', { class: 'scrim', onclick: () => { S.drawer = false; render() } }))
  const main = h('main', { class: 'main', id: 'main' })
  root.append(main)
  renderSidebar()
  if (S.view === 'new') main.append(topbar('New agent'), newAgentView())
  else if (S.view === 'isolations') main.append(topbar('Isolations'), h('div', { class: 'page', id: 'isolations' }))
  else if (S.view === 'services') main.append(topbar('Services'), h('div', { class: 'page', id: 'services' }))
  else main.append(agentView())
  if (S.view === 'isolations') renderIsolations()
  if (S.view === 'services') renderServices()
  renderToast()
}

function topbar(title, sub, ...extra) {
  return h('header', { class: 'topbar', id: 'topbar' },
    h('button', { class: 'menu-btn', 'aria-label': 'Agents', onclick: () => { S.drawer = true; render() } }, '☰'),
    h('div', { class: 'title' }, h('h1', null, title), sub ? h('div', { class: 'sub' }, sub) : null),
    ...extra)
}

function renderSidebar() {
  const el = document.getElementById('sidebar')
  if (!el) return
  const live = S.agents.filter((a) => !a.archived_at)
  const archived = S.agents.filter((a) => a.archived_at)
  const row = (a) => {
    const st = a.archived_at ? 'idle' : a.state?.status || 'idle'
    const meta = a.problem ? h('span', { class: 'problem' }, '⚠ ' + a.problem) : `${a.kind} · ${a.isolation} · ${st}`
    return h('button', {
      class: 'agent' + (a.id === S.sel && S.view === 'agent' ? ' selected' : '') + (a.archived_at ? ' archived' : ''),
      onclick: () => select(a.id),
    }, h('span', { class: 'dot ' + st }), h('span', { class: 'name' }, a.name), h('span', { class: 'meta' }, meta))
  }
  el.replaceChildren(
    h('div', { class: 'brand' }, 'gero', h('span', { class: 'count' }, `${live.length} agent${live.length === 1 ? '' : 's'}`)),
    h('button', { class: 'new-agent' + (S.view === 'new' ? ' active' : ''), onclick: () => show('new') }, '+ New agent'),
    h('div', { class: 'agent-list' },
      live.map(row),
      h('button', { class: 'fold', onclick: () => { S.showArchived = !S.showArchived; renderSidebar() } },
        `${S.showArchived ? '▾' : '▸'} Archived (${archived.length})`),
      S.showArchived ? archived.map(row) : null),
    h('div', { class: 'side-foot' },
      h('button', { class: S.view === 'isolations' ? 'active' : '', onclick: () => show('isolations') }, '⚙ Isolations'),
      h('button', { class: S.view === 'services' ? 'active' : '', onclick: () => show('services') }, '⚿ Services')),
  )
}

function agentView() {
  const a = agentById(S.sel)
  S.drawn = null
  if (!a) {
    const frag = document.createDocumentFragment()
    frag.append(topbar('gero'), h('div', { class: 'page' }, h('div', { class: 'empty' },
      h('p', null, S.agents.length ? 'No agent selected.' : 'No agents yet.'),
      h('button', { class: 'btn primary', onclick: () => show('new') }, 'Start an agent'))))
    return frag
  }
  S.drawn = a.id
  const frag = document.createDocumentFragment()
  frag.append(
    h('div', { id: 'topbar-slot' }),
    h('div', { class: 'convo', id: 'convo' }, h('div', { class: 'convo-inner', id: 'convo-inner' })),
    h('div', { id: 'approvals' }),
    composer(a),
  )
  queueMicrotask(() => { renderTopbar(); renderConvo(true); renderApprovals() })
  return frag
}

function renderTopbar() {
  const slot = document.getElementById('topbar-slot')
  const a = agentById(S.sel)
  if (!slot || !a) return
  const st = a.archived_at ? 'archived' : a.state?.status || 'idle'
  const working = st === 'working'
  // replaceChildren turns null into the text "null"; keep only nodes.
  slot.replaceChildren(...[
    topbar(a.name, `${a.kind} · ${a.isolation} · ${a.id}`,
      h('span', { class: 'chip ' + st }, st),
      h('div', { class: 'actions' },
        working ? h('button', { class: 'btn', onclick: () => guarded(() => api('POST', `/v1/agents/${a.id}/interrupt`)) }, '■', h('span', { class: 'label' }, ' Stop')) : null,
        a.archived_at ? null : h('button', {
          class: 'btn ghost', title: 'Archive', onclick: async () => {
            if (!confirm(`Archive "${a.name}"? Its history is kept.`)) return
            await guarded(() => api('POST', `/v1/agents/${a.id}/archive`))
            S.sel = null
            await guarded(refreshAgents)
            render()
          },
        }, '🗄', h('span', { class: 'label' }, ' Archive')))),
    a.problem ? h('div', { class: 'banner' }, a.problem) : null,
  ].filter(Boolean))
}

function renderConvo(forceBottom) {
  const box = document.getElementById('convo')
  const inner = document.getElementById('convo-inner')
  const a = agentById(S.sel)
  if (!box || !inner || !a) return
  const atBottom = forceBottom || box.scrollHeight - box.scrollTop - box.clientHeight < 80
  const c = convo(a.id)
  const items = c.entries.map((en) => entryView(en, a)).filter(Boolean)
  inner.replaceChildren(...(items.length ? items : [h('div', { class: 'empty' }, 'Loading…')]))
  if (atBottom) box.scrollTop = box.scrollHeight
}

function entryView(en, a) {
  switch (en.kind) {
    case 'user': return h('div', { class: 'msg-user text' }, richText(en.text))
    case 'assistant':
      return h('div', { class: 'msg-agent' + (en.live ? ' live' : '') }, h('div', { class: 'who' }, label(a)), h('div', { class: 'text' }, richText(en.text)))
    case 'reasoning': return h('div', { class: 'reasoning' }, en.text)
    case 'tool': {
      const mark = { ok: '✓', failed: '✗', running: '…' }[en.state]
      const out = (en.output || '').trim()
      return h('details', { class: 'tool' },
        h('summary', null, h('span', { class: 'mark ' + en.state }, mark), h('span', { class: 'tname' }, en.name), h('span', { class: 'tsum' }, en.summary)),
        out ? h('pre', null, clip(out, 40)) : null)
    }
    case 'approval': {
      const d = en.decision
      const outcome = d ? ' — ' + decisionText(d) : ''
      return h('div', { class: 'approval' + (d ? ' decided' : '') },
        h('div', { class: 'head' }, `Approval: ${en.akind}${outcome}`),
        h('div', { class: 'what' }, en.summary),
        en.reason && !d ? h('div', { class: 'why' }, en.reason) : null)
    }
    case 'interrupted': return h('div', { class: 'interrupted' }, '⏸ This turn was interrupted. Send a message to continue.')
    case 'error': return h('div', { class: 'error-line' }, '✗ ' + en.text)
    case 'notice': return h('div', { class: 'notice' }, en.text)
  }
  return null
}

function renderApprovals() {
  const box = document.getElementById('approvals')
  const a = agentById(S.sel)
  if (!box || !a) return
  const p = pending(convo(a.id))
  if (!p.length) { box.replaceChildren(); return }
  const ap = p[0]
  box.replaceChildren(h('div', { class: 'composer pending-approval' },
    h('div', { class: 'composer-row' }, h('div', { class: 'approval grow' },
      h('div', { class: 'head' }, `${label(a)} asks to ${askText(ap.akind)}` + (p.length > 1 ? ` (+${p.length - 1} more)` : '')),
      h('div', { class: 'what' }, ap.summary),
      ap.reason ? h('div', { class: 'why' }, ap.reason) : null,
      h('div', { class: 'actions' },
        h('button', { class: 'btn primary', onclick: () => decide(ap.id, 'accept') }, 'Accept'),
        h('button', { class: 'btn', onclick: () => decide(ap.id, 'decline') }, 'Decline'),
        h('button', { class: 'btn danger', onclick: () => decide(ap.id, 'cancel') }, 'Decline & stop'))))))
}

function composer(a) {
  const ta = h('textarea', {
    rows: 1,
    placeholder: a.archived_at ? 'This agent is archived.' : 'Message the agent…',
    title: coarse ? null : 'Enter sends · Shift+Enter adds a line',
    disabled: !!a.archived_at,
    value: S.drafts.get(a.id) || '',
  })
  const grow = () => { ta.style.height = 'auto'; ta.style.height = Math.min(ta.scrollHeight, 200) + 'px' }
  const submit = async () => {
    const text = ta.value.trim()
    if (!text) return
    ta.value = ''
    S.drafts.set(a.id, '')
    grow()
    await send(text)
  }
  ta.addEventListener('input', grow)
  ta.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' && !e.shiftKey && !coarse && !e.isComposing) { e.preventDefault(); submit() }
    if (e.key === 'Escape') { S.settingsOpen = false; renderSettings(a) }
  })
  queueMicrotask(grow)
  const wrap = h('div', { class: 'composer' },
    h('div', { id: 'settings-slot' }),
    h('div', { class: 'composer-row' },
      ta,
      h('button', {
        class: 'icon-btn' + (S.settingsOpen ? ' on' : ''), 'aria-label': 'Agent settings', title: 'Agent settings',
        disabled: !!a.archived_at, onclick: () => { S.settingsOpen = !S.settingsOpen; renderSettings(a) },
      }, '⚙'),
      h('button', { class: 'btn primary send-btn', disabled: !!a.archived_at, onclick: submit }, 'Send')))
  queueMicrotask(() => renderSettings(a))
  return wrap
}

function renderSettings(a) {
  const slot = document.getElementById('settings-slot')
  if (!slot) return
  document.querySelector('.composer .icon-btn')?.classList.toggle('on', S.settingsOpen)
  if (!S.settingsOpen) { slot.replaceChildren(); return }
  const o = a.options || {}
  const effort = h('select', null, efforts(a.kind).map((e) => h('option', { value: e, selected: (o.effort || '') === e }, e || 'default')))
  const model = h('input', { value: o.model || '', placeholder: "the CLI's default" })
  const approvals = h('select', null,
    h('option', { value: 'never', selected: (o.approvals || 'never') === 'never' }, 'never — run unattended'),
    h('option', { value: 'ask', selected: o.approvals === 'ask' }, 'ask — the agent asks first'))
  slot.replaceChildren(h('div', { class: 'popover' },
    h('h3', null, 'Agent settings'),
    h('div', { class: 'field' }, h('label', null, 'Effort'), effort),
    h('div', { class: 'field' }, h('label', null, 'Model'), model),
    h('div', { class: 'field' }, h('label', null, 'Approvals'), approvals),
    h('div', { class: 'note' }, a.kind === 'claude' ? 'Claude restarts with these when it is idle.' : 'Codex uses these from the next turn.'),
    h('div', { class: 'actions popover-actions' },
      h('button', {
        class: 'btn primary', onclick: async () => {
          const opts = { effort: effort.value || undefined, model: model.value.trim() || undefined, approvals: approvals.value }
          await guarded(async () => {
            await api('PUT', `/v1/agents/${a.id}/options`, opts)
            S.settingsOpen = false
            renderSettings(a)
            toast('Settings saved')
            await refreshAgents()
          })
        },
      }, 'Save'),
      h('button', { class: 'btn', onclick: () => { S.settingsOpen = false; renderSettings(a) } }, 'Cancel'))))
}

function newAgentView() {
  let kind = 'claude'
  const err = h('p', { class: 'form-error' })
  const iso = h('select')
  const noIso = h('p', { class: 'form-error' }, 'There is no isolation yet. ',
    h('a', { href: '#/isolations', onclick: (e) => { e.preventDefault(); show('isolations') } }, 'Create one'), ' first.')
  // The list may arrive after the form is drawn; refill it in place so a
  // half-typed prompt survives.
  const fillIsos = () => {
    const cur = iso.value
    iso.replaceChildren(...S.isolations.map((i) => h('option', { value: i.name, selected: i.name === cur }, i.name)))
    noIso.hidden = S.isolations.length > 0
  }
  fillIsos()
  S.refillIsolations = fillIsos
  const repo = h('input', { placeholder: 'https://github.com/owner/repo (optional)', autocomplete: 'off' })
  const effort = h('select')
  const model = h('input', { placeholder: "optional; the CLI's default otherwise" })
  const approvals = h('select', null,
    h('option', { value: 'never' }, 'never — run unattended inside the isolation'),
    h('option', { value: 'ask' }, 'ask — the agent asks first for what its own rules flag'))
  const prompt = h('textarea', { placeholder: 'What should the agent do?' })
  const fillEfforts = () => effort.replaceChildren(...efforts(kind).map((e) => h('option', { value: e }, e || 'default')))
  fillEfforts()
  const seg = h('div', { class: 'segmented' })
  const drawSeg = () => seg.replaceChildren(...['claude', 'codex'].map((k) =>
    h('button', { type: 'button', class: k === kind ? 'on' : '', onclick: () => { kind = k; drawSeg(); fillEfforts() } }, k === 'codex' ? 'Codex' : 'Claude')))
  drawSeg()
  const create = h('button', { class: 'btn primary' }, 'Start agent')
  create.addEventListener('click', async () => {
    err.textContent = ''
    if (!iso.value) { err.textContent = 'Create an isolation first.'; return }
    if (!prompt.value.trim()) { err.textContent = 'An initial prompt is required.'; prompt.focus(); return }
    create.disabled = true
    create.textContent = 'Starting…'
    try {
      const a = await api('POST', '/v1/agents', {
        isolation: iso.value, kind, prompt: prompt.value.trim(), repo: repo.value.trim() || undefined,
        options: { effort: effort.value || undefined, model: model.value.trim() || undefined, approvals: approvals.value },
      })
      await refreshAgents()
      select(a.id)
    } catch (e) {
      if (e instanceof Unauthorized) { S.authed = false; render(); return }
      err.textContent = e.message
      create.disabled = false
      create.textContent = 'Start agent'
    }
  })
  queueMicrotask(() => prompt.focus())
  return h('div', { class: 'page' }, h('div', { class: 'page-inner' },
    h('h2', null, 'New agent'),
    h('p', { class: 'lead' }, 'It runs inside an isolation, on its own clone of the code.'),
    noIso,
    h('div', { class: 'row' },
      h('div', { class: 'field' }, h('label', null, 'Isolation'), iso),
      h('div', { class: 'field' }, h('label', null, 'Agent'), seg)),
    h('div', { class: 'field' }, h('label', null, 'Prompt'), prompt),
    h('div', { class: 'field' }, h('label', null, 'Repository'), repo, h('span', { class: 'hint' }, 'Cloned into the agent’s workspace inside the isolation.')),
    h('div', { class: 'row' },
      h('div', { class: 'field' }, h('label', null, 'Effort'), effort),
      h('div', { class: 'field' }, h('label', null, 'Model'), model)),
    h('div', { class: 'field' }, h('label', null, 'Approvals'), approvals),
    err,
    create))
}

function renderIsolations() {
  const page = document.getElementById('isolations')
  if (!page) return
  const fenceText = (i) => i.fence_state === 'verified' ? 'network fence verified' : i.fence_state === 'off' ? 'network fence off' : null
  const card = (i) => {
    const key = h('input', { placeholder: 'VARIABLE', autocomplete: 'off' })
    const val = h('input', { type: 'password', placeholder: 'value (empty removes it)', autocomplete: 'new-password' })
    return h('div', { class: 'card' },
      h('div', { class: 'head' },
        h('h3', null, i.name),
        h('span', { class: 'chip ' + (i.state === 'running' ? '' : 'blocked') }, i.state),
        h('span', { class: 'chip ' + (i.fence_state === 'verified' ? '' : i.fence_state === 'off' ? 'blocked' : 'error') }, fenceText(i) || 'fence ✗')),
      i.fence_state !== 'verified' && i.fence_state !== 'off' ? h('div', { class: 'fence-bad' }, i.fence_state) : null,
      h('div', { class: 'facts' }, `${i.image} · network ${i.network || 'default'} · env: ${(i.env_keys || []).join(', ') || '—'}`),
      S.openForm === `verify:${i.name}` && S.verifyOut ? verifyView(S.verifyOut) : null,
      exposuresView(i),
      h('div', { class: 'inline-form' }, key, val,
        h('button', {
          class: 'btn', onclick: async () => {
            const k = key.value.trim()
            if (!k) return
            await guarded(async () => {
              await api('PATCH', `/v1/isolations/${i.name}/env`, { [k]: val.value === '' ? null : val.value })
              toast(`${k} saved; idle agents restart with it`)
              await refreshIsolations()
            })
          },
        }, 'Set')),
      h('div', { class: 'actions' },
        h('button', { class: 'btn', disabled: S.verifying === i.name, onclick: () => verifyIsolation(i) },
          S.verifying === i.name ? 'Measuring…' : 'Verify'),
        h('button', {
          class: 'btn', onclick: async () => {
            if (!confirm(`Upgrade ${i.name} to the current image? Running agents restart; sessions are kept.`)) return
            toast('Upgrading…')
            await guarded(async () => { await api('POST', `/v1/isolations/${i.name}/upgrade`); toast(`${i.name} upgraded`); await refreshIsolations() })
          },
        }, 'Upgrade'),
        h('button', {
          class: 'btn danger', onclick: async () => {
            if (!confirm(`Delete isolation ${i.name}? Its volumes are kept.`)) return
            await guarded(async () => { await api('DELETE', `/v1/isolations/${i.name}`); toast(`${i.name} deleted`); await refreshIsolations() })
          },
        }, 'Delete')),
      h('p', { class: 'note' }, 'Log an agent CLI in from a terminal on the Mac: ', h('code', null, `gero iso login ${i.name} codex`),
        ' — or give Claude a token from ', h('code', null, 'claude setup-token'), ' as CLAUDE_CODE_OAUTH_TOKEN.'))
  }
  const name = h('input', { placeholder: 'name', autocomplete: 'off' })
  const memory = h('input', { value: '4G' })
  const fence = h('input', { type: 'checkbox', checked: true })
  const create = h('button', { class: 'btn primary' }, 'Create isolation')
  create.addEventListener('click', async () => {
    if (!name.value.trim()) return
    create.disabled = true
    create.textContent = 'Creating… (the first one builds the image)'
    await guarded(async () => {
      await api('POST', '/v1/isolations', { name: name.value.trim(), memory: memory.value.trim(), fence: fence.checked ? 'on' : 'off' })
      toast('Isolation created')
      await refreshIsolations()
    })
    create.disabled = false
    create.textContent = 'Create isolation'
  })
  page.replaceChildren(h('div', { class: 'page-inner' },
    h('h2', null, 'Isolations'),
    h('p', { class: 'lead' }, 'Each runs in its own VM, fenced off the Mac and the LAN, with internet access.'),
    S.isolations.map(card),
    h('div', { class: 'card' },
      h('div', { class: 'head' }, h('h3', null, 'New isolation')),
      h('div', { class: 'row spaced' },
        h('div', { class: 'field' }, h('label', null, 'Name'), name),
        h('div', { class: 'field' }, h('label', null, 'Memory'), memory)),
      h('label', { class: 'check' }, fence, 'Network fence (agents start only behind a verified fence)'),
      h('div', { class: 'actions' }, create))))
}

// ---- Services: the credential proxies' environments

const DEFAULT = 'default'
const enc = encodeURIComponent
const envPath = (svc, name) => `/v1/envs/${enc(svc)}/${enc(name)}`

function rerender() {
  if (S.view === 'services') renderServices()
  else if (S.view === 'isolations') renderIsolations()
}
function openForm(key) { S.openForm = key; rerender() }
function closeForm() { S.openForm = null; S.login = null; S.checkOut = null; S.verifyOut = null; rerender() }

// fieldControls draws a service's fields, the same list the CLI builds its
// flags from, and reads them back as the CLI's edit: what was given is set,
// a list emptied is cleared, the rest is left alone.
function fieldControls(fields, cur, adding) {
  cur = cur || {}
  const rows = []
  const readers = []
  for (const f of fields || []) {
    const v = cur[f.key]
    let ctl
    let read
    if (f.kind === 'bool') {
      const opts = adding || typeof v !== 'boolean' ? [DEFAULT, 'on', 'off'] : ['on', 'off']
      const sel = typeof v === 'boolean' ? (v ? 'on' : 'off') : DEFAULT
      ctl = h('select', null, opts.map((o) => h('option', { value: o, selected: o === sel }, o)))
      read = (p) => { if (ctl.value === 'on') p.set[f.key] = true; else if (ctl.value === 'off') p.set[f.key] = false }
    } else if (f.kind === 'list' && f.choices?.length) {
      const have = new Set(Array.isArray(v) ? v : [])
      const boxes = f.choices.map((c) => ({ c, el: h('input', { type: 'checkbox', checked: have.has(c) }) }))
      ctl = h('div', { class: 'ticks' }, boxes.map(({ c, el }) => h('label', { class: 'check' }, el, c)))
      read = (p) => {
        const got = boxes.filter((b) => b.el.checked).map((b) => b.c)
        if (got.length) p.set[f.key] = got
        else if (have.size) p.clear.push(f.key)
      }
    } else if (f.kind === 'list') {
      const have = Array.isArray(v) ? v : []
      ctl = h('input', { value: have.join(', '), placeholder: (f.metavar || 'value') + ', …', autocomplete: 'off' })
      read = (p) => {
        const got = ctl.value.split(',').map((x) => x.trim()).filter(Boolean)
        if (got.length) p.set[f.key] = got
        else if (have.length) p.clear.push(f.key)
      }
    } else if (f.kind === 'string' && f.choices?.length) {
      ctl = h('select', null, [DEFAULT, ...f.choices].map((o) => h('option', { value: o, selected: o === (v || DEFAULT) }, o)))
      read = (p) => { if (ctl.value !== DEFAULT) p.set[f.key] = ctl.value }
    } else if (f.kind === 'int') {
      ctl = h('input', { type: 'number', value: typeof v === 'number' ? String(v) : '', placeholder: f.metavar || '' })
      read = (p) => {
        if (!ctl.value.trim()) return
        const n = Number(ctl.value)
        if (!Number.isInteger(n)) throw new Error(`${f.key} must be a whole number`)
        p.set[f.key] = n
      }
    } else {
      // A value stored in a shape the field does not spell (git's pinned
      // token command) is left as it is unless something is typed.
      const odd = v != null && typeof v !== 'string'
      ctl = h('input', { value: typeof v === 'string' ? v : '', placeholder: odd ? '(unchanged)' : f.metavar || '', autocomplete: 'off' })
      read = (p) => { const t = ctl.value.trim(); if (t) p.set[f.key] = t }
    }
    rows.push(h('div', { class: 'field' }, h('label', null, f.key), ctl, f.help ? h('span', { class: 'hint' }, f.help) : null))
    readers.push(read)
  }
  return { rows, patch: () => { const p = { set: {}, clear: [] }; readers.forEach((r) => r(p)); return p } }
}

function compactConfig(cfg) {
  return Object.keys(cfg || {}).sort().map((k) => `${k}=${Array.isArray(cfg[k]) ? cfg[k].join(',') : cfg[k]}`).join(' ')
}

function renderServices() {
  const page = document.getElementById('services')
  if (!page) return
  page.replaceChildren(h('div', { class: 'page-inner' },
    h('h2', null, 'Services'),
    h('p', { class: 'lead' }, 'Credentials stay on the Mac; isolations get proxies that use them within an allowlist. Define an environment here, then turn it on for an isolation on the Isolations page.'),
    S.services.length ? S.services.map(serviceCard) : h('div', { class: 'empty' }, 'Loading…')))
}

function serviceCard(s) {
  const envs = S.envs.filter((e) => e.service === s.name)
  const key = 'new:' + s.name
  return h('div', { class: 'card' },
    h('div', { class: 'head' }, h('h3', null, s.name), h('span', { class: 'note' }, s.summary)),
    envs.map((e) => envRow(s, e)),
    S.openForm === key ? envForm(s, null) : h('div', { class: 'actions' }, h('button', { class: 'btn', onclick: () => openForm(key) }, '+ New environment')))
}

function secretChip(s, e) {
  if (!s.secrets?.length) return null
  if (e.secret_error) return h('span', { class: 'chip error' }, 'secret unreadable')
  if (e.missing_secrets?.length) return h('span', { class: 'chip blocked' }, s.login ? 'not logged in' : 'no secret')
  return h('span', { class: 'chip' }, s.login ? 'logged in' : 'secret set')
}

function envRow(s, e) {
  const k = (what) => `${what}:${s.name}:${e.name}`
  let open = null
  if (S.openForm === k('edit')) open = envForm(s, e)
  else if (S.openForm === k('secret')) open = secretForm(s, e)
  else if (S.openForm === k('login') && S.login) open = loginForm(s, e)
  else if (S.openForm === k('check') && S.checkOut) {
    open = h('div', { class: 'out' + (S.checkOut.error ? ' bad' : '') },
      h('pre', null, (S.checkOut.output || '') + (S.checkOut.error ? '✗ ' + S.checkOut.error : '')),
      h('button', { class: 'btn', onclick: closeForm }, 'Close'))
  }
  const exposed = e.exposed_to?.length ? 'on for ' + e.exposed_to.join(', ') : 'not on for any isolation'
  return h('div', { class: 'env' },
    h('div', { class: 'head' }, h('strong', null, e.name), secretChip(s, e), h('span', { class: 'note' }, exposed)),
    h('div', { class: 'facts' }, compactConfig(e.config) || '—'),
    open || h('div', { class: 'actions' },
      h('button', { class: 'btn', onclick: () => openForm(k('edit')) }, 'Edit'),
      s.secrets?.length ? h('button', {
        class: 'btn', onclick: () => s.login ? startLogin(s, e, k('login')) : openForm(k('secret')),
      }, s.login ? 'Log in' : 'Secret') : null,
      h('button', { class: 'btn', onclick: () => checkEnv(s, e, k('check')) }, 'Check'),
      h('button', {
        class: 'btn danger', onclick: async () => {
          const on = e.exposed_to?.length ? ` ${e.exposed_to.join(', ')} stop serving it.` : ''
          if (!confirm(`Remove ${s.name} env ${e.name}?${on} Its secret stays until cleared.`)) return
          await guarded(async () => {
            await api('DELETE', envPath(s.name, e.name) + (on ? '?force=1' : ''))
            toast(`Removed ${s.name}/${e.name}`)
            await refreshServices()
          })
        },
      }, 'Remove')))
}

function envForm(s, e) {
  const name = e ? null : h('input', { placeholder: 'letters, digits, - and _', autocomplete: 'off' })
  const ctl = fieldControls(s.fields, e ? e.config : {}, !e)
  const err = h('p', { class: 'form-error' })
  const save = h('button', { class: 'btn primary' }, 'Save')
  save.addEventListener('click', async () => {
    err.textContent = ''
    const n = e ? e.name : name.value.trim()
    if (!n) { err.textContent = 'A name is required.'; return }
    try {
      const v = await api(e ? 'PATCH' : 'POST', envPath(s.name, n), ctl.patch())
      S.openForm = null
      toast([`${s.name}/${n} saved`, ...(v.warnings || [])].join(' — '))
      await refreshServices()
      renderServices()
    } catch (x) {
      if (x instanceof Unauthorized) { S.authed = false; render(); return }
      err.textContent = x.message
    }
  })
  return h('div', { class: 'subform' },
    name ? h('div', { class: 'field' }, h('label', null, 'name'), name, h('span', { class: 'hint' }, 'what isolations expose')) : null,
    ctl.rows, err,
    h('div', { class: 'actions' }, save, h('button', { class: 'btn', onclick: closeForm }, 'Cancel')))
}

function secretForm(s, e) {
  const inputs = s.secrets.map((f) => ({ f, el: h('input', { type: 'password', autocomplete: 'new-password' }) }))
  const err = h('p', { class: 'form-error' })
  const save = h('button', { class: 'btn primary' }, 'Save')
  save.addEventListener('click', async () => {
    err.textContent = ''
    const body = Object.fromEntries(inputs.map(({ f, el }) => [f, el.value]))
    try {
      await api('PUT', envPath(s.name, e.name) + '/secret', body)
      S.openForm = null
      toast(`Stored [${s.name} ${e.name}] on the Mac; proxies read it on their next request`)
      await refreshServices()
      renderServices()
    } catch (x) {
      if (x instanceof Unauthorized) { S.authed = false; render(); return }
      err.textContent = x.message
    }
  })
  return h('div', { class: 'subform' },
    inputs.map(({ f, el }) => h('div', { class: 'field' }, h('label', null, f), el)),
    h('p', { class: 'note' }, 'Written to the Mac’s credentials file (0600) and never shown again.'), err,
    h('div', { class: 'actions' }, save,
      h('button', {
        class: 'btn', onclick: async () => {
          if (!confirm(`Delete the stored secret for ${s.name}/${e.name}?`)) return
          await guarded(async () => { await api('DELETE', envPath(s.name, e.name) + '/secret'); S.openForm = null; toast('Secret removed'); await refreshServices(); renderServices() })
        },
      }, 'Clear'),
      h('button', { class: 'btn', onclick: closeForm }, 'Cancel')))
}

async function startLogin(s, e, key) {
  await guarded(async () => {
    S.login = await api('POST', envPath(s.name, e.name) + '/login', {})
    openForm(key)
  })
}

// The login lands the browser on a loopback address on the Mac, which a
// phone (or any other device) cannot reach: the page fails to load, and its
// address, pasted here, finishes the login.
function loginForm(s, e) {
  const l = S.login
  const pasted = h('input', { placeholder: `${l.redirect}?code=…`, autocomplete: 'off' })
  const err = h('p', { class: 'form-error' })
  const finish = h('button', { class: 'btn primary' }, 'Finish login')
  finish.addEventListener('click', async () => {
    err.textContent = ''
    if (!pasted.value.trim()) { err.textContent = 'Paste the address the browser landed on.'; return }
    try {
      await api('POST', envPath(s.name, e.name) + '/login/' + enc(l.id), { callback: pasted.value.trim() })
      S.openForm = null
      S.login = null
      toast(`Logged in; [${s.name} ${e.name}] is stored on the Mac`)
      await refreshServices()
      renderServices()
    } catch (x) {
      if (x instanceof Unauthorized) { S.authed = false; render(); return }
      err.textContent = x.message + ' — start the login again if it keeps failing.'
    }
  })
  return h('div', { class: 'subform' },
    h('p', null, '1. ', h('a', { href: l.url, target: '_blank', rel: 'noopener noreferrer' }, `Open the ${s.name} login`), ' and approve the access.'),
    h('p', null, '2. The page it sends you to will not load. Copy that page’s address and paste it here:'),
    h('div', { class: 'field' }, pasted), err,
    h('div', { class: 'actions' }, finish, h('button', { class: 'btn', onclick: closeForm }, 'Cancel')))
}

async function checkEnv(s, e, key) {
  toast(`Checking ${s.name}/${e.name}…`)
  await guarded(async () => {
    S.checkOut = await api('POST', envPath(s.name, e.name) + '/check')
    S.toast = null
    renderToast()
    openForm(key)
  })
}

// ---- Verify: an isolation's guarantees, measured now (gero iso verify)

async function verifyIsolation(i) {
  S.verifying = i.name
  rerender()
  await guarded(async () => {
    S.verifyOut = await api('POST', `/v1/isolations/${enc(i.name)}/verify`)
    S.openForm = `verify:${i.name}`
  })
  S.verifying = null
  rerender()
}

// verifyView is a report: FAIL is a broken guarantee, WARN something set
// up wrong, each with its reason.
function verifyView(v) {
  const failed = v.checks.filter((c) => !c.ok)
  const broken = failed.filter((c) => c.severity === 'isolation')
  const kind = (c) => c.ok ? 'pass' : c.severity === 'isolation' ? 'fail' : 'warn'
  const summary = failed.length === 0 ? `${v.checks.length} checks passed`
    : `${failed.length} of ${v.checks.length} failed` + (broken.length ? ` — ${broken.length} broken guarantee${broken.length > 1 ? 's' : ''}` : ' — setup only')
  return h('div', { class: 'subform verify' },
    h('div', { class: 'verify-sum ' + (broken.length ? 'fail' : failed.length ? 'warn' : 'pass') }, summary),
    h('ul', { class: 'checks' }, v.checks.map((c) => h('li', { class: kind(c) },
      h('span', { class: 'mark' }, { pass: 'PASS', fail: 'FAIL', warn: 'WARN' }[kind(c)]),
      h('span', { class: 'what' }, c.check),
      h('span', { class: 'got' }, c.got),
      !c.ok && c.detail ? h('div', { class: 'why' }, c.detail) : null))),
    h('div', { class: 'actions' }, h('button', { class: 'btn', onclick: closeForm }, 'Close')))
}

// ---- An isolation's services

function exposuresView(i) {
  const xs = S.exposures[i.name]
  if (!xs || !S.services.length) return null
  const byName = Object.fromEntries(xs.map((x) => [x.service, x]))
  return h('div', { class: 'svc-list' },
    h('div', { class: 'svc-title' }, 'Services', h('span', { class: 'note' }, ' — the credentials stay on the Mac')),
    S.services.map((s) => {
      const x = byName[s.name] || { service: s.name, enabled: false, envs: [] }
      const key = `expose:${i.name}:${s.name}`
      const detail = x.enabled
        ? [(x.envs || []).join(', ') || 'no env', x.default_env ? `(default ${x.default_env})` : '', compactConfig(x.settings)].filter(Boolean).join(' ')
        : 'off'
      return h('div', { class: 'svc-row' },
        h('div', { class: 'svc-line' },
          h('span', { class: 'dot' + (x.enabled ? ' on' : '') }),
          h('strong', null, s.name), h('span', { class: 'facts' }, detail),
          x.missing?.length ? h('span', { class: 'chip error' }, 'missing ' + x.missing.join(',')) : null,
          S.openForm === key ? null : h('button', { class: 'btn small', onclick: () => openForm(key) }, x.enabled ? 'Change' : 'Turn on')),
        S.openForm === key ? exposeForm(i, s, x) : null)
    }))
}

function exposeForm(i, s, x) {
  const names = S.envs.filter((e) => e.service === s.name).map((e) => e.name)
  const have = new Set(x.envs || [])
  const boxes = names.map((n) => ({ n, el: h('input', { type: 'checkbox', checked: have.has(n) }) }))
  const def = h('select', null, [DEFAULT, ...names].map((n) => h('option', { value: n, selected: n === (x.default_env || DEFAULT) }, n)))
  const ctl = fieldControls(s.settings, x.settings || {}, !x.enabled)
  const err = h('p', { class: 'form-error' })
  const apply = h('button', { class: 'btn primary' }, x.enabled ? 'Apply' : 'Turn on')
  apply.addEventListener('click', async () => {
    err.textContent = ''
    try {
      const envs = boxes.filter((b) => b.el.checked).map((b) => b.n)
      await api('PUT', `/v1/isolations/${enc(i.name)}/services/${enc(s.name)}`,
        { envs, default_env: def.value === DEFAULT ? undefined : def.value, settings: ctl.patch() })
      S.openForm = null
      toast(`${s.name} is on for ${i.name}; agents pick it up at their next idle moment`)
      await refreshIsolations()
      renderIsolations()
    } catch (e) {
      if (e instanceof Unauthorized) { S.authed = false; render(); return }
      err.textContent = e.message
    }
  })
  return h('div', { class: 'subform' },
    h('div', { class: 'field' }, h('label', null, 'Environments'),
      names.length ? h('div', { class: 'ticks' }, boxes.map(({ n, el }) => h('label', { class: 'check' }, el, n)))
        : h('span', { class: 'hint' }, 'none defined yet — ', h('a', { href: '#/services', onclick: (ev) => { ev.preventDefault(); show('services') } }, 'define one')),
      h('span', { class: 'hint' }, 'None ticked: the service with no credential (git: public repositories only).')),
    names.length > 1 ? h('div', { class: 'field' }, h('label', null, 'Default'), def, h('span', { class: 'hint' }, 'what the plain variables point at')) : null,
    ctl.rows, err,
    h('div', { class: 'actions' }, apply,
      x.enabled ? h('button', {
        class: 'btn danger', onclick: async () => {
          if (!confirm(`Turn ${s.name} off for ${i.name}? Its files and variables are removed there.`)) return
          await guarded(async () => {
            await api('DELETE', `/v1/isolations/${enc(i.name)}/services/${enc(s.name)}`)
            S.openForm = null
            toast(`${s.name} is off for ${i.name}`)
            await refreshIsolations()
            renderIsolations()
          })
        },
      }, 'Turn off') : null,
      h('button', { class: 'btn', onclick: closeForm }, 'Cancel')))
}

function loginView() {
  const tok = h('input', { type: 'password', placeholder: 'token', autocomplete: 'off' })
  const err = h('p', { class: 'form-error' })
  const go = async () => {
    const res = await fetch('/v1/session', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ token: tok.value }) })
    if (!res.ok) { err.textContent = 'That token is not valid.'; return }
    S.authed = true
    render()
    guarded(refreshAgents)
  }
  tok.addEventListener('keydown', (e) => { if (e.key === 'Enter') go() })
  return h('div', { class: 'login' },
    h('h2', null, 'Sign in to gero'),
    h('p', { class: 'note' }, 'Run ', h('code', null, 'gero web'), ' on the Mac: it opens a signed-in link, and prints one for your phone.'),
    h('div', { class: 'field' }, h('label', null, 'Or paste the token'), tok),
    err,
    h('button', { class: 'btn primary', onclick: go }, 'Sign in'))
}

function renderToast() {
  document.querySelector('.toast')?.remove()
  if (S.toast) document.body.append(h('div', { class: 'toast' + (S.toast.err ? ' err' : ''), role: 'status' }, S.toast.msg))
}

document.addEventListener('keydown', (e) => {
  if (e.key === 'Escape' && S.drawer) { S.drawer = false; render() }
})

route()
startPolling()
