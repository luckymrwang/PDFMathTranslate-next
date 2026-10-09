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
    wx: { navigateTo: () => navigations++, showToast() {},
      getStorageSync: () => ({}), setStorageSync() {} },
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
  page.data.filePath = 'ready.pdf'
  page.data.fileName = 'ready.pdf'
  page.data.mode = 'enhanced'
  page.data.engine = 'GPT-6'
  page.data.orderReady = false
  page.data.countingPages = false
  page.data.paying = false
  page.preparedFile = { path: 'ready.pdf', page_count: 16, file_token: 'ready',
    expires_at: Date.now() / 1000 + 1800 }
  let configRequests = 0, orderCreations = 0
  api.getPayConfig = async () => { configRequests++; return { enabled: true } }
  api.createPreparedOrder = async () => {
    orderCreations++
    return { order: { id: 'quote-' + orderCreations, quantity: 16, unit_price_fen: 20, total_fen: 320 },
      payData: { signData: 'server-payload' } }
  }
  const configWarmup = page.getPaymentConfig()
  await configWarmup
  const opening = page.openPaymentSheet()
  assert.equal(page.data.paymentTotal, '8.00', 'Show the page test amount as soon as page count is known')
  assert.equal(page.data.orderReady, false, 'An early display amount does not enable payment')
  await opening
  assert.equal(configRequests, 1, 'Reuse the background payment config check')
  assert.equal(orderCreations, 0, 'Opening the payment preview must not create an unpaid order')
  page.closePaymentSheet()
  await page.openPaymentSheet()
  assert.equal(orderCreations, 0, 'Repeated preview visits must not accumulate orders')
  await page.confirmTranslation()
  assert.equal(orderCreations, 1, 'Create an order only for an explicit payment action')
  assert.equal(page.data.unitPrice, '0.50')
  assert.equal(page.data.paymentTotal, '8.00', 'Keep display test amount stable when backend test pricing differs')
  assert.equal(page.payData.signData, 'server-payload', 'Keep the actual server payment payload unchanged')
  assert.equal(page.data.orderReady, true)
  page.closePaymentSheet()
  api.createPreparedOrder = async () => { throw new Error('测试失败') }
  await page.openPaymentSheet()
  await page.confirmTranslation()
  assert.equal(page.data.paymentError, '测试失败')
  assert.equal(page.data.countingPages, false)
  assert.equal(page.data.orderReady, true)
  api.createPreparedOrder = async () => ({ order: { id: 'retry-quote', quantity: 16 },
    payData: { signData: 'retry' } })
  await page.confirmTranslation()
  assert.equal(page.quotedJob.orderID, 'retry-quote')
  assert.equal(page.data.paymentError, '')
  const storedJobs = { 'cancelled-order': { name: 'Original.pdf', paymentCancelledAt: Date.now() } }
  sandbox.wx.getStorageSync = key => key === 'virtualpay_pending_jobs' ? storedJobs : []
  let orderState = 'pending'
  api.listPayOrders = async () => ({ orders: [{ id: 'cancelled-order', name: 'temporary.pdf',
    state: orderState, engine: 'Bing', lang_in: 'en', lang_out: 'zh', created_at: 1 }] })
  await page.loadOrders()
  assert.equal(page.data.recentOrders.length, 0, 'Unpaid orders must not appear in recent translations')
  orderState = 'paid'
  await page.loadOrders()
  assert.equal(page.data.recentOrders[0].name, 'Original.pdf')
  let notice
  sandbox.wx.showToast = ({ title }) => { notice = title }
  api.queryPayOrder = async () => { orderState = 'closed'; return { id: 'cancelled-order', state: 'closed' } }
  const before = navigations
  await page.openPaidOrder({ currentTarget: { dataset: { index: 0 } } })
  assert.equal(page.data.recentOrders.length, 0, 'Closed orders must leave the homepage')
  assert.ok(notice.includes('订单已关闭'))
  assert.ok(!notice.includes('退款'))
  assert.equal(navigations, before, 'Cancelled/closed orders cannot start translation')
  assert.equal(page.checkingOrder, false)
  let reusedPath, expiredPrompt
  let fileInfo = ({ filePath, success }) => { reusedPath = filePath; success({ size: 2048 }) }
  sandbox.wx.getFileSystemManager = () => ({ getFileInfo: args => fileInfo(args) })
  api.preparePdf = async () => ({ file_token: 'reorder-file', page_count: 16, expires_at: Date.now() / 1000 + 1800 })
  api.warmEngine = async () => ({ ready: true })
  page.data.engines = ['GPT-6']
  page.data.engine = 'GPT-6'
  page.availableEngines = new Set(['GPT-6'])
  page.data.countingPages = false
  page.data.paying = false
  const createdBeforeRestore = orderCreations
  await page.restoreReorderFile({ name: 'Original.pdf', filePath: 'cached.pdf', engine: 'GPT-6' }, Promise.resolve())
  assert.equal(reusedPath, 'cached.pdf')
  assert.equal(page.data.paymentSheet, true)
  assert.equal(page.data.orderReady, true)
  assert.equal(page.paymentIntent.fileToken, 'reorder-file')
  assert.equal(page.quotedJob, null, 'Old order credentials must not be reused')
  assert.equal(page.payData, null)
  assert.equal(orderCreations, createdBeforeRestore, 'Restoring a file cannot create or pay an order')
  sandbox.wx.showModal = data => { expiredPrompt = data }
  fileInfo = ({ fail }) => fail({ errMsg: 'file not found' })
  await page.restoreReorderFile({ name: 'Expired.pdf', filePath: 'gone.pdf', engine: 'GPT-6' }, Promise.resolve())
  assert.equal(expiredPrompt.confirmText, '选择文件')
  assert.ok(expiredPrompt.content.includes('Expired.pdf'))
  assert.equal(orderCreations, createdBeforeRestore)
  let serverPrepares = 0, duplicateUploads = 0, localChecks = 0
  api.prepareOrderPdf = async id => {
    assert.equal(id, 'old-server-order')
    serverPrepares++
    return { file_token: 'server-reuse-' + serverPrepares, page_count: 16, file_name: 'Original.pdf', file_size: 2048, expires_at: Date.now() / 1000 + 1800 }
  }
  api.preparePdf = async () => { duplicateUploads++; throw new Error('Unexpected PDF upload') }
  fileInfo = () => { localChecks++; throw new Error('Unexpected local file check') }
  page.data.countingPages = false
  await page.restoreReorderFile({ id: 'old-server-order', name: 'Original.pdf', engine: 'GPT-6' }, Promise.resolve())
  assert.equal(serverPrepares, 1)
  assert.equal(duplicateUploads, 0)
  assert.equal(localChecks, 0)
  assert.equal(page.data.paymentSheet, true)
  assert.equal(page.data.orderReady, true)
  assert.equal(page.paymentIntent.fileToken, 'server-reuse-1')
  page.preparedFile.expires_at = 0
  await page.prepareSelectedFile()
  assert.equal(serverPrepares, 2, 'Expired server credentials renew without uploading')
  assert.equal(duplicateUploads, 0)
  assert.equal(orderCreations, createdBeforeRestore)
  api.prepareOrderPdf = async () => { throw Object.assign(new Error('Missing server file'), { statusCode: 410 }) }
  fileInfo = ({ success }) => success({ size: 2048 })
  api.preparePdf = async () => { duplicateUploads++; return { file_token: 'fallback-upload', page_count: 16, expires_at: Date.now() / 1000 + 1800 } }
  await page.restoreReorderFile({ id: 'old-server-order', name: 'Original.pdf', filePath: 'fallback.pdf', engine: 'GPT-6' }, Promise.resolve())
  assert.equal(duplicateUploads, 1, 'Only missing server files fall back to a local upload')
  assert.equal(page.sourceOrderID, null)
  assert.equal(page.paymentIntent.fileToken, 'fallback-upload')
  vm.runInNewContext(fs.readFileSync(path.join(root, 'pages/history/history.js'), 'utf8'), { ...sandbox })
  page.setData = values => Object.assign(page.data, values)
  page.alive = true
  await page.loadOrders()
  assert.equal(page.data.orders[0].label, '已关闭')
  assert.equal(page.data.orders[0].canReorder, true)
  assert.equal(page.data.orders[0].name, 'Original.pdf')
  let relaunched = false
  sandbox.wx.reLaunch = () => { relaunched = true }
  page.reorder({ currentTarget: { dataset: { index: 0 } } })
  assert.equal(relaunched, true)
  assert.equal(app.globalData.translationReorder.id, 'cancelled-order')
  orderState = 'pending'
  await page.loadOrders()
  assert.equal(page.data.orders[0].label, '支付操作已取消')
  assert.equal(page.data.orders[0].canReorder, false, 'Pending payment proof must be resolved before repurchase')
  console.log('PASS: payment proof, lifecycle, upload reuse, stable test amounts, prefetch, retry')
}
run().catch(error => { console.error(error); process.exitCode = 1 })
