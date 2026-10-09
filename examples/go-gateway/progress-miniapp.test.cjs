// Run: node progress-miniapp.test.cjs /absolute/path/to/pdftranslate
const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const vm = require('node:vm')
const root = process.argv[2]
assert.ok(root, 'Provide the miniapp directory')

function harness(getStatus) {
  let page, timer, starts = 0
  const api = { getStatus, startPayTranslation: async () => { starts++; return { id: 'task' } } }
  vm.runInNewContext(fs.readFileSync(path.join(root, 'pages/progress/progress.js'), 'utf8'), {
    require: name => name.includes('api') ? api : { defaultTranslate: {} },
    Page: value => { page = value },
    setTimeout: (fn, delay) => { timer = { fn, delay }; return 1 },
    clearTimeout: () => { timer = null }, clearInterval() {},
    wx: {},
  })
  page.setData = values => Object.assign(page.data, values)
  Object.assign(page, { alive: true, visible: true, job: { id: 'task', orderID: 'existing-order' } })
  page.data.progress = 14
  return { page, timer: () => timer, starts: () => starts }
}

async function run() {
  let fail = true
  const h = harness(async () => {
    if (fail) throw { errMsg: 'request:fail timeout' }
    return { progress: 25, state: 'running' }
  })
  for (const delay of [2000, 4000, 8000, 16000, 30000, 30000]) {
    await h.page.fetchStatus()
    assert.equal(h.timer().delay, delay)
    assert.equal(h.page.data.progress, 14)
    assert.equal(h.page.data.error, '')
    assert.equal(h.page.data.canRetry, true)
  }
  fail = false
  await h.page.fetchStatus()
  assert.equal(h.page.data.progress, 25)
  assert.equal(h.page.data.connectionHint, '')
  assert.equal(h.page.queryFailures, 0)
  assert.equal(h.timer().delay, 2000)
  assert.equal(h.starts(), 0, 'Reconnect must never submit a task')
  h.page.onHide()
  assert.equal(h.timer(), null)
  await h.page.fetchStatus()
  assert.equal(h.timer(), null)
  h.page.onShow()
  await new Promise(resolve => setImmediate(resolve))
  assert.equal(h.timer().delay, 2000)
  h.page.onUnload()
  assert.equal(h.timer(), null)

  for (const statusCode of [403, 404]) {
    const terminal = harness(async () => { throw { statusCode } })
    await terminal.page.fetchStatus()
    assert.equal(terminal.page.terminal, true)
    assert.equal(terminal.timer(), null)
    assert.ok(terminal.page.data.error)
  }
  const failed = harness(async () => ({ progress: 14, state: 'error', error: 'Engine failed' }))
  await failed.page.fetchStatus()
  assert.equal(failed.page.data.error, 'Engine failed')
  assert.equal(failed.timer(), null)

  let resolve, calls = 0
  const pending = harness(() => { calls++; return new Promise(r => { resolve = r }) })
  const request = pending.page.fetchStatus()
  await pending.page.fetchStatus()
  pending.page.retryStatus()
  assert.equal(calls, 1, 'No overlapping polls')
  pending.page.onUnload()
  resolve({ progress: 50, state: 'running' })
  await request
  assert.equal(pending.page.data.progress, 14)
  assert.equal(pending.timer(), null)
  console.log('Progress reconnect tests passed')
}
run().catch(error => { console.error(error); process.exitCode = 1 })
