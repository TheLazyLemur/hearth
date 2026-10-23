'use strict'
// hearth in a browser: one WebSocket speaking the daemon's protocol through the bridge, and a
// touch-first view of it. S is this tab's copy of the daemon's state, kept as client.go keeps
// the TUI's.

const version = document.documentElement.dataset.version
const S = { sessions: [], live: {}, backends: {}, models: {}, notice: '', conn: 'connecting', paired: null, epoch: 0, archived: false, sheet: null }
const dirty = {} // by session: the first entry changed since its transcript was drawn
const pending = new Map()
const md = markdownit() // its defaults escape raw HTML and refuse javascript: and data: links
let ws, nextId = 0, retry = 500, timer

// ---- protocol

function connect() {
  clearTimeout(timer)
  S.conn = 'connecting'
  const url = new URL('ws', location.href)
  url.protocol = location.protocol === 'https:' ? 'wss:' : 'ws:'
  url.hash = ''
  ws?.close()
  const sock = new WebSocket(url)
  ws = sock
  let greeted = false
  sock.onopen = () => sock.send(JSON.stringify({ hello: { version } }))
  sock.onmessage = ev => {
    const m = JSON.parse(ev.data)
    if (m.snapshot) {
      take(m.snapshot)
      greeted = true
      retry = 500
      S.conn = 'open'
    } else if (m.change) applyChange(m.change)
    else if (m.reply) answer(m.reply)
    else if (m.hello) S.notice = `The daemon speaks v${m.hello.version}, this page v${version}` + (m.hello.busy ? ', and it is running turns' : '')
    else if (m.bye) S.notice = 'Disconnected: ' + m.bye
    draw()
  }
  sock.onclose = async () => {
    if (ws !== sock) return // a newer connection replaced it
    for (const p of pending.values()) p.reject(new Error('disconnected'))
    pending.clear()
    S.conn = 'closed'
    if (!greeted && !(await checkPaired())) return draw()
    draw()
    timer = setTimeout(connect, retry)
    retry = Math.min(retry * 2, 10000)
  }
}

// checkPaired asks whether this browser's cookie still pairs it; an unreachable server
// counts as paired, so the page keeps retrying.
async function checkPaired() {
  try {
    S.paired = (await fetch('pair')).status !== 403
  } catch {
    S.paired = true
  }
  return S.paired
}

async function pair(code) {
  const res = await fetch('pair', { method: 'POST', body: new URLSearchParams({ token: code.trim() }) })
  if (!res.ok) {
    S.paired = false
    S.notice = 'That pairing code is wrong'
    return draw()
  }
  S.paired = true
  S.notice = ''
  connect()
}

function take(snap) {
  S.sessions = snap.sessions || []
  S.live = snap.live || {}
  S.backends = snap.backends || {}
  S.models = snap.models || {}
  if (snap.notice) S.notice = snap.notice
  S.epoch++ // every transcript is drawn afresh
}

function applyChange(ch) {
  if (ch.deleted) {
    delete S.live[ch.deleted]
    S.sessions = S.sessions.filter(s => s.sessionId !== ch.deleted)
    return
  }
  if (ch.notice != null) S.notice = ch.notice
  if (ch.models) S.models[ch.models.backend] = ch.models.items // null while it lists
  const s = ch.session
  if (!s) return
  const i = S.sessions.findIndex(o => o.sessionId === s.sessionId)
  if (i >= 0) S.sessions[i] = s
  else S.sessions.unshift(s) // newest first, as the daemon keeps them
  const l = ch.live
  if (!l) return
  if (ch.entries) {
    const old = S.live[s.sessionId]?.Entries || []
    l.Entries = old.slice(0, ch.entries.from).concat(ch.entries.items || [])
    dirty[s.sessionId] = Math.min(dirty[s.sessionId] ?? Infinity, ch.entries.from)
  }
  S.live[s.sessionId] = l
}

function answer(r) {
  const p = pending.get(r.id)
  if (!p) return
  pending.delete(r.id)
  if (r.err) p.reject(new Error(r.err))
  else p.resolve(r.result)
}

function call(method, args = {}) {
  if (ws?.readyState !== WebSocket.OPEN) return Promise.reject(new Error('Not connected'))
  const id = ++nextId
  ws.send(JSON.stringify({ request: { id, method, args } }))
  return new Promise((resolve, reject) => pending.set(id, { resolve, reject }))
}

// act runs a request and shows its error as the notice; it resolves undefined on failure.
function act(method, args) {
  return call(method, args).catch(e => {
    S.notice = e.message
    draw()
  })
}

// ---- state, as core reads it

function statusOf(s) {
  const l = S.live[s.sessionId]
  if (l?.Perms?.length) return 'waiting'
  if (l?.Running) return 'running'
  if (l?.Errored) return 'error'
  return 'idle'
}

const rank = { waiting: 3, running: 2, error: 1, idle: 0 }
const label = { waiting: 'Needs you', running: 'Working', error: 'Failed', idle: '' }

// root is the session the user started that s works under: s itself unless s is an agent.
function root(s) {
  while (s.spawnedFrom?.kind === 'agent') {
    const p = S.sessions.find(o => o.sessionId === s.spawnedFrom.sessionId)
    if (!p) return s // its parent was deleted: it stands alone
    s = p
  }
  return s
}

function visible() {
  return S.sessions
    .filter(s => s.isArchived === S.archived && root(s) === s)
    .sort((a, b) => (b.isStarred ? 1 : 0) - (a.isStarred ? 1 : 0) || b.lastActivityAt - a.lastActivityAt)
}

function agentsOf(r) {
  return S.sessions.filter(s => s !== r && root(s) === r).sort((a, b) => a.createdAt - b.createdAt)
}

function questions(p) {
  return (p.Input?.questions || []).map(q => ({ question: q.question, options: (q.options || []).map(o => o.label) }))
}

// ---- drawing

function h(tag, props, ...kids) {
  const el = document.createElement(tag)
  for (const [k, v] of Object.entries(props || {})) {
    if (k.startsWith('on')) el.addEventListener(k.slice(2), v)
    else if (k === 'class') el.className = v
    else if (v != null && v !== false) el.setAttribute(k, v === true ? '' : v)
  }
  el.append(...kids.flat(Infinity).filter(k => k != null && k !== false && k !== ''))
  return el
}

// keep rebuilds el only when sig changes, so a tap isn't lost to a redraw of the same thing.
function keep(el, sig, build) {
  if (el.dataset.sig === sig) return
  el.dataset.sig = sig
  el.replaceChildren(...[build()].flat().filter(Boolean))
}

const base = p => (p || '').split('/').filter(Boolean).pop() || p || ''
const href = s => '#s/' + encodeURIComponent(s.sessionId)

function ago(ms) {
  if (!ms) return ''
  const m = Math.round((Date.now() - ms) / 60000)
  return m < 1 ? 'now' : m < 60 ? m + 'm' : m < 1440 ? Math.round(m / 60) + 'h' : Math.round(m / 1440) + 'd'
}

let queued = false
function draw() {
  if (queued) return
  queued = true
  requestAnimationFrame(() => {
    queued = false
    render()
  })
}

const app = document.getElementById('app')
const toast = h('div', { class: 'notice', role: 'status', hidden: true, onclick: () => { S.notice = ''; draw() } })
const overlay = h('div', { class: 'overlay' })
document.body.append(toast, overlay)
let view = { key: null }

function render() {
  const id = location.hash.startsWith('#s/') ? decodeURIComponent(location.hash.slice(3)) : null
  if (id && S.conn === 'open' && !S.sessions.some(s => s.sessionId === id)) {
    location.hash = '' // deleted, or from another hearth home
    return
  }
  const key = S.paired === false ? 'pair' : id ? 's/' + id : 'list'
  if (view.key !== key) {
    if (S.sheet?.settings) S.sheet = null // it belongs to the session being left
    view = { key, ...(key === 'pair' ? pairView() : id ? sessionView(id) : listView()) }
    app.replaceChildren(view.el)
  }
  view.update?.()
  toast.textContent = S.notice
  toast.hidden = !S.notice
  const ss = S.sheet?.settings && S.sessions.find(o => o.sessionId === S.sheet.settings)
  const top = overlay.querySelector('.sheet')?.scrollTop // a pick redraws the sheet; stay where the tap was
  keep(overlay, JSON.stringify([S.sheet, S.sheet && S.sessions.length,
    ss && [ss.model, ss.effort, ss.permissionMode, S.live[ss.sessionId]?.Model, S.models[ss.backend]]]), () => S.sheet && sheet())
  if (top) overlay.querySelector('.sheet').scrollTop = top
}

function pairView() {
  const input = h('input', { placeholder: 'Pairing code', autocomplete: 'off', autocapitalize: 'off', spellcheck: 'false', 'aria-label': 'Pairing code' })
  const el = h('div', { class: 'view pair' },
    h('h1', {}, 'hearth'),
    h('p', {}, 'Run ', h('code', {}, 'hearth web'), ' on your computer, then open the pairing link it prints or paste its code here.'),
    h('form', { onsubmit: e => { e.preventDefault(); pair(input.value) } }, input, h('button', { class: 'primary' }, 'Pair')))
  return { el }
}

function listView() {
  const dot = h('span', { class: 'dot', role: 'img' })
  const rows = h('ul', { class: 'sessions' })
  const toggle = h('button', { class: 'link', onclick: () => { S.archived = !S.archived; draw() } })
  const el = h('div', { class: 'view' },
    h('header', {}, h('h1', {}, 'hearth'), dot,
      h('button', { class: 'primary', onclick: () => { S.sheet = 'new'; draw() } }, 'New')),
    h('div', { class: 'scroll' }, rows, toggle))
  return {
    el,
    update() {
      dot.className = 'dot ' + S.conn
      dot.setAttribute('aria-label', S.conn)
      const items = visible().map(s => {
        const agents = agentsOf(s)
        const st = [s, ...agents].map(statusOf).reduce((a, b) => (rank[b] > rank[a] ? b : a))
        const meta = [base(s.originCwd), s.branch && '⎇ ' + s.branch, S.backends[s.backend || 'claude']?.tag,
          agents.length && agents.length + (agents.length === 1 ? ' agent' : ' agents'), ago(s.lastActivityAt)]
        return { s, st, meta: meta.filter(Boolean).join(' · ') }
      })
      keep(rows, JSON.stringify([S.conn, S.archived, items.map(i => [i.s.sessionId, i.s.title, i.s.isStarred, i.st, i.meta])]), () =>
        items.length
          ? items.map(({ s, st, meta }) => h('li', {}, h('a', { href: href(s), class: 'row' },
              h('span', { class: 'title' }, (s.isStarred ? '★ ' : '') + (s.title || 'Untitled')),
              label[st] && h('span', { class: 'pill ' + st }, label[st]),
              h('span', { class: 'meta' }, meta))))
          : h('li', { class: 'empty' }, S.conn !== 'open' ? 'Connecting…' : S.archived ? 'Nothing archived' : 'No sessions yet'))
      toggle.textContent = S.archived ? 'Back to sessions' : 'Archived'
    },
  }
}

function sessionView(id) {
  const title = h('h1', {})
  const sub = h('div', { class: 'sub' })
  const pick = h('button', { class: 'pick', 'aria-haspopup': 'dialog', onclick: () => openSettings(id) })
  const tabs = h('nav', { class: 'tabs', 'aria-label': 'Agents' })
  const log = h('div', { class: 'log' })
  const queue = h('div', { class: 'log queued' })
  const working = h('div', { class: 'working', 'aria-live': 'polite' })
  const card = h('div', {})
  const scroller = h('div', { class: 'scroll' }, log, queue, working, card)
  const input = h('textarea', { rows: 1, placeholder: 'Message', 'aria-label': 'Message' })
  const stop = h('button', { class: 'danger', onclick: () => act('interrupt', { session: id }) }, 'Stop')
  const send = h('button', { class: 'primary', onclick: submit }, 'Send')
  const grow = () => {
    input.style.height = 'auto'
    input.style.height = Math.min(input.scrollHeight, 160) + 'px'
  }
  input.addEventListener('input', grow)
  input.addEventListener('keydown', e => {
    if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) {
      e.preventDefault()
      submit()
    }
  })
  const el = h('div', { class: 'view' },
    h('header', {}, h('a', { href: '#', class: 'back', 'aria-label': 'Sessions' }, '‹'),
      h('div', { class: 'head' }, title, sub), pick),
    tabs, scroller,
    h('div', { class: 'composer' }, input, stop, send))

  // a pending question takes the composer's text as its answer, as the TUI's input does
  const asking = () => !!S.live[id]?.Perms?.[0]?.Answers
  function submit() {
    const text = input.value.trim()
    if (!text) return
    input.value = ''
    grow()
    call(asking() ? 'answerQuestion' : 'send', { session: id, text }).catch(e => {
      input.value = text
      grow()
      S.notice = e.message
      draw()
    })
  }

  let shown = 0, drawnEpoch = -1, askedEpoch = -1
  return {
    el,
    update() {
      const s = S.sessions.find(o => o.sessionId === id)
      if (!s) return
      const l = S.live[id] || {}
      if (!l.Loaded && askedEpoch !== S.epoch && S.conn === 'open') {
        askedEpoch = S.epoch
        act('ensureLoaded', { session: id })
      }
      const caps = S.backends[s.backend || 'claude']
      keep(title, s.title || 'Untitled', () => s.title || 'Untitled')
      const mode = (s.permissionMode || 'default') !== 'default' && modes.find(m => m[0] === s.permissionMode)?.[1]
      keep(sub, JSON.stringify([s.originCwd, s.branch, caps, mode]), () =>
        [base(s.originCwd), s.branch && '⎇ ' + s.branch, caps?.label, mode].filter(Boolean).join(' · '))
      const cur = s.model || l.Model, named = S.models[s.backend]?.find(m => m.ID === cur)?.Name
      const chosen = (named || cur || 'Model') + (s.effort ? ' · ' + s.effort : '')
      keep(pick, chosen, () => [chosen, h('span', { 'aria-hidden': 'true' }, ' ▾')])

      const r = root(s), agents = agentsOf(r)
      keep(tabs, JSON.stringify([agents.length && [r, ...agents].map(a => [a.sessionId, a.title, statusOf(a)])]), () =>
        agents.length && [r, ...agents].map(a => h('a', { href: href(a), class: 'tab ' + statusOf(a) + (a.sessionId === id ? ' on' : '') }, a.title || 'Untitled')))
      tabs.hidden = !agents.length

      const atBottom = scroller.scrollHeight - scroller.scrollTop - scroller.clientHeight < 80
      const es = l.Entries || []
      const from = Math.min(drawnEpoch === S.epoch ? (dirty[id] ?? shown) : 0, shown, es.length)
      delete dirty[id]
      while (log.children.length > from) log.lastChild.remove()
      log.append(...es.slice(from).map(entry))
      keep(queue, JSON.stringify(l.Queued || []), () => (l.Queued || []).map(entry))
      if (drawnEpoch === -1 || atBottom) scroller.scrollTop = scroller.scrollHeight
      shown = es.length
      drawnEpoch = S.epoch

      working.textContent = l.Running ? (l.Verb || 'Working') + '…' : ''
      const p = l.Perms?.[0]
      keep(card, p ? p.RequestID + ':' + Object.keys(p.Answers || {}).length : '', () => p && permCard(id, p))
      input.placeholder = asking() ? 'Type your own answer' : l.Running ? 'Queue a message' : 'Message'
      stop.hidden = !l.Running
      send.disabled = S.conn !== 'open'
    },
  }
}

function entry(e) {
  switch (e.Kind) {
    case 'user': return h('div', { class: 'msg user' }, e.Text)
    case 'assistant': {
      const el = h('div', { class: 'msg md' })
      el.innerHTML = md.render(e.Text || '')
      for (const a of el.querySelectorAll('a')) a.target = '_blank' // following a link mustn't drop the session
      return el
    }
    case 'tool': return h('div', { class: 'tool', onclick: ev => ev.currentTarget.classList.toggle('open') }, e.Text)
    case 'result': {
      const lines = (e.Text || '').trimEnd().split('\n')
      const summary = e.Err ? 'Failed' : lines.length > 1 ? lines.length + ' lines' : lines[0] || 'Done'
      return h('details', { class: 'result' + (e.Err ? ' err' : '') }, h('summary', {}, summary), h('pre', {}, e.Text))
    }
    case 'error': return h('div', { class: 'msg error' }, e.Text)
    case 'interrupt': return h('div', { class: 'info' }, 'Interrupted')
  }
  return h('div', { class: 'info' }, e.Text)
}

function permCard(id, p) {
  if (p.Answers) {
    const qs = questions(p), n = Object.keys(p.Answers).length, q = qs[n]
    if (!q) return null
    return h('section', { class: 'card' },
      h('div', { class: 'card-title' }, q.question, h('span', { class: 'count' }, ` ${n + 1}/${qs.length}`)),
      h('div', { class: 'choices' },
        // by number, as core reads an option, so an option worded like a number stays itself
        q.options.map((o, i) => h('button', { onclick: () => act('answerQuestion', { session: id, text: String(i + 1) }) }, o))),
      h('div', { class: 'hint' }, 'Or type your own answer below'))
  }
  const [what, body] = describe(p)
  const sug = p.Suggestions?.[0]
  return h('section', { class: 'card' },
    h('div', { class: 'card-title' }, what),
    p.Input?.description && h('div', { class: 'hint' }, p.Input.description),
    h('pre', {}, body),
    p.Reason && h('div', { class: 'warn' }, 'Flagged by auto mode: ' + p.Reason),
    h('div', { class: 'choices' },
      h('button', { class: 'primary', onclick: () => act('answerPerm', { session: id, allow: true }) }, 'Yes'),
      sug && h('button', { onclick: () => act('answerPerm', { session: id, allow: true, always: true }) }, alwaysLabel(sug)),
      h('button', { class: 'danger', onclick: () => act('answerPerm', { session: id, allow: false }) }, 'No')))
}

function describe(p) {
  const i = p.Input || {}
  switch (p.Tool) {
    case 'Bash': return ['Bash command', i.command]
    case 'Edit': case 'MultiEdit': case 'NotebookEdit': return ['Edit file', summarise(i)]
    case 'Write': return ['Create file', summarise(i)]
    case 'Read': return ['Read file', summarise(i)]
    case 'ExitPlanMode': return ['Ready to code?', i.plan]
  }
  return [p.Tool, summarise(i)]
}

// summarise picks the one tool argument that says what a call does, as core.SummariseInput does.
function summarise(i) {
  for (const k of ['command', 'file_path', 'pattern', 'url', 'query', 'description', 'prompt']) {
    if (typeof i[k] === 'string' && i[k]) return i[k]
  }
  return JSON.stringify(i)
}

function alwaysLabel(sug) {
  if (sug.type === 'addRules') return 'Yes, and don’t ask again for: ' + (sug.rules || []).map(r => r.ruleContent || r.toolName).join(', ')
  if (sug.type === 'setMode' && sug.mode === 'acceptEdits') return 'Yes, allow all edits this session'
  return 'Yes, and don’t ask again'
}

// modes are the permission modes, as the TUI cycles them, with the names it shows.
const modes = [['default', 'Default'], ['acceptEdits', 'Accept edits'], ['plan', 'Plan'], ['auto', 'Auto']]
const efforts = ['low', 'medium', 'high', 'xhigh', 'max']

function openSettings(id) {
  const s = S.sessions.find(o => o.sessionId === id)
  if (S.models[s.backend] === undefined) {
    S.models[s.backend] = null // listing; the list arrives as a change, as client.go asks
    act('models', { session: id })
  }
  S.sheet = { settings: id }
  draw()
}

// settings sets a session's model, effort and mode, each tap at once, so the sheet stays open.
function settings(id) {
  const s = S.sessions.find(o => o.sessionId === id)
  if (!s) return []
  const ms = S.models[s.backend], current = s.model || S.live[id]?.Model
  const listed = ms?.find(m => m.ID === current)
  // a model the list doesn't name may take any level, as the TUI's /effort allows
  const levels = ms == null ? [] : listed ? listed.Efforts || [] : efforts
  const opt = (on, onclick, ...kids) => h('button', { 'aria-pressed': on ? 'true' : 'false', onclick }, ...kids)
  // the short rows first: a backend can list a dozen models
  return [
    h('h2', {}, 'Permission mode'),
    h('div', { class: 'opts' }, modes.map(([m, name]) =>
      opt((s.permissionMode || 'default') === m, () => act('setMode', { session: id, mode: m }), name))),
    levels.length > 0 && [h('h2', {}, 'Effort'), h('div', { class: 'opts' },
      opt(!s.effort, () => act('setEffort', { session: id, effort: '' }), 'Default'),
      levels.map(e => opt(s.effort === e, () => act('setEffort', { session: id, effort: e }), e)))],
    h('h2', {}, 'Model'),
    ms == null ? h('p', { class: 'hint' }, 'Loading models…')
      : !ms.length ? h('p', { class: 'hint' }, 'This backend lists no models')
      : h('ul', { class: 'dirs' }, ms.map(m => h('li', {},
          opt(m.ID === current, () => act('setModel', { session: id, model: m.ID }),
            h('span', { class: 'title' }, m.Name || m.ID), m.Description && h('span', { class: 'meta' }, m.Description))))),
  ]
}

function sheet() {
  const close = () => { S.sheet = null; draw() }
  const body = S.sheet === 'new' ? pickFolder() : S.sheet.settings ? settings(S.sheet.settings) : [
    h('h2', {}, 'Work in an isolated git worktree?'),
    h('p', { class: 'hint' }, S.sheet.dir),
    h('div', { class: 'choices' },
      h('button', { class: 'primary', onclick: () => create(S.sheet.dir, true) }, 'Yes, in a worktree'),
      h('button', { onclick: () => create(S.sheet.dir, false) }, 'No, in the folder itself')),
  ]
  return h('div', { class: 'backdrop', onclick: e => e.target === e.currentTarget && close() },
    h('section', { class: 'sheet', role: 'dialog', 'aria-modal': 'true' }, body,
      h('button', { class: 'link', onclick: close }, S.sheet.settings ? 'Done' : 'Cancel')))
}

// shortcut: only folders hearth already has sessions in, so the daemon is never handed a path
// nobody checked; take a typed path once the daemon checks the folder exists
function pickFolder() {
  const dirs = [...new Set([...S.sessions].sort((a, b) => b.lastActivityAt - a.lastActivityAt).map(s => s.originCwd).filter(Boolean))]
  return [
    h('h2', {}, 'New session in…'),
    dirs.length
      ? h('ul', { class: 'dirs' }, dirs.map(d => h('li', {}, h('button', { onclick: () => pick(d) }, h('span', { class: 'title' }, base(d)), h('span', { class: 'meta' }, d)))))
      : h('p', { class: 'hint' }, 'Start a session from the terminal first; new ones can start in any folder hearth has seen.'),
  ]
}

async function pick(dir) {
  const repo = await act('isRepo', { dir })
  if (repo === undefined) return
  if (repo) {
    S.sheet = { dir }
    return draw()
  }
  create(dir, false)
}

async function create(dir, worktree) {
  S.sheet = null
  draw()
  const id = await act('newSession', { dir, worktree })
  if (id) location.hash = 's/' + encodeURIComponent(id)
}

// ---- start

// pairLink pairs with the code in a pairing link, opened fresh or into this tab.
function pairLink() {
  const code = location.hash.match(/^#pair=(\w+)$/)?.[1]
  if (!code) return false
  history.replaceState(null, '', location.pathname + location.search) // keep the code out of history
  pair(code)
  return true
}

addEventListener('hashchange', () => pairLink() || draw())
document.addEventListener('visibilitychange', () => {
  // a phone drops the socket while the page is hidden; come back at once, not on the backoff
  if (!document.hidden && S.paired && ws?.readyState === WebSocket.CLOSED) connect()
})
if (!pairLink()) checkPaired().then(ok => (ok ? connect() : draw()))
draw()
