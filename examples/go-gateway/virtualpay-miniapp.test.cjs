// Run: node virtualpay-miniapp.test.cjs /absolute/path/to/pdftranslate
const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const vm = require('node:vm')
const root = process.argv[2]
assert.ok(root, 'Provide the miniapp directory')
const payment = require(path.join(root, 'utils/payment.js'))

async function run() {
  let captured
  global.wx = {
    canIUse: () => true,
    getAppBaseInfo: () => ({ version: '8.0.68' }),
    getDeviceInfo: () => ({ platform: 'ios' }),
    requestVirtualPayment: data => { captured = data; data.success({}) },
  }
  const payData = { mode: 'short_series_goods', signData: '{"b":2, "a":1}', paySig: 'server-signature', signature: 'user-signature' }
  await payment.pay(payData)
  assert.equal(captured.signData, payData.signData)
  wx.getAppBaseInfo = () => ({ version: '8.0.67' })
  assert.throws(() => payment.assertPaymentSupported(), /8.0.68/)
  wx.getDeviceInfo = () => ({ platform: 'android' })
  payment.assertPaymentSupported()
  wx.canIUse = () => false
  assert.throws(() => payment.assertPaymentSupported(), /不支持/)
  assert.equal(payment.formatFen(160), '1.60')
  assert.equal(payment.formatFen(1.5), '—')
  assert.equal(payment.compareVersion('8.0.100', '8.0.68'), 1)

  let page, navigations = 0, queries = 0
  const app = { globalData: {} }
  let serverState = 'pending'
  const api = {
    queryPayOrder: async () => { queries++; return { state: serverState } },
    listPayOrders: async () => ({ orders: [] }),
  }
  const sandbox = {
    require: name => {
      if (name === '../../config') return { demoMode: false }
      if (name === '../../utils/api') return api
      if (name === '../../utils/payment') return { ...payment, assertPaymentSupported() {}, pay: async () => ({}) }
      return {}
    },
    Page: value => { page = value },
    wx: { navigateTo: () => navigations++, showToast() {} },
    getApp: () => app,
    setTimeout: callback => { callback(); return 0 },
    console,
  }
  vm.runInNewContext(fs.readFileSync(path.join(root, 'pages/index/index.js'), 'utf8'), sandbox)
  page.setData = values => Object.assign(page.data, values)
  page.availableEngines = new Set(['GPT-6'])
  page.data.orderReady = true
  page.quotedJob = { orderID: 'server-order', engine: 'GPT-6' }
  await page.confirmTranslation()
  assert.equal(queries, 30)
  assert.equal(navigations, 0, 'Client success cannot grant an unpaid order')
  serverState = 'paid'
  await page.confirmTranslation()
  assert.equal(navigations, 1)
  assert.equal(app.globalData.translation.orderID, 'server-order')
  page.unloaded = true
  await page.confirmTranslation()
  assert.equal(navigations, 1, 'Do not navigate after page destruction')
  page.unloaded = false
  page.fileRevision = 1
  page.data.filePath = 'first.pdf'
  let uploads = 0, complete
  api.preparePdf = () => {
    uploads++
    return new Promise(resolve => { complete = resolve })
  }
  const first = page.prepareSelectedFile()
  const again = page.prepareSelectedFile()
  assert.equal(first, again, 'Concurrent callers must share an upload')
  complete({ file_token: 'file-1', page_count: 16, expires_at: Date.now() / 1000 + 1800 })
  await first
  await page.prepareSelectedFile()
  assert.equal(uploads, 1, 'Ready files must be reused')
  page.data.filePath = 'second.pdf'
  page.fileRevision++
  const stale = page.prepareSelectedFile()
  page.removeFile()
  complete({ file_token: 'stale', page_count: 99, expires_at: Date.now() / 1000 + 1800 })
  await assert.rejects(stale, /文件已变更/)
  assert.equal(page.preparedFile, null, 'Removed file must not return from an old request')
  console.log('PASS: versions, integer fen, signed payload, server payment proof, lifecycle')
}
run().catch(error => { console.error(error); process.exitCode = 1 })
