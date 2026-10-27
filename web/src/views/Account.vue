<script setup>
import { ref, reactive, computed, onMounted, onUnmounted } from 'vue'
import api, { setAccountId } from '@/api'
import { useAppStore } from '@/stores/app'
import { useAccountStore } from '@/stores/account'
import { useRouter } from 'vue-router'

const app = useAppStore()
const account = useAccountStore()
const router = useRouter()

const accounts = ref([])
const activeId = ref('')          // 当前 active 账号（来自 GET /api/accounts/active）
const sheet = ref('')             // '' | manage | add | qr
const editing = ref(null)
const delTarget = ref(null)
const limitText = computed(() => {
  const n = account.userInfo?.accountLimit
  if (n === -1) return '无限'
  return String(n || 2)
})

/* ---------- 加载账号列表 + 当前 active ---------- */
async function loadAccounts() {
  try {
    const { data } = await api.get('/api/accounts')
    accounts.value = (data && data.data) || []
  } catch (e) { accounts.value = [] }
  const ids = new Set(accounts.value.map((a) => String(a.id)))
  let cur = ''
  try {
    const { data } = await api.get('/api/accounts/active')
    cur = data && data.data && data.data.accountId ? String(data.data.accountId) : ''
  } catch (e) { cur = '' }
  if (!cur || !ids.has(cur)) {
    cur = accounts.value[0] ? String(accounts.value[0].id) : ''
  }
  activeId.value = cur
  if (cur) {
    setAccountId(cur)
    account.switchAccount(cur)
  } else {
    setAccountId('')
  }
}

/* ---------- 切换当前账号（热切换：不整页刷新，广播事件让各页面重拉数据） ---------- */
async function switchAcc(id) {
  if (String(id) === String(activeId.value)) return
  try {
    const { data } = await api.post('/api/accounts/active', { id })
    if (data?.ok) {
      account.switchAccount(id)
      window.dispatchEvent(new CustomEvent('account-switched', { detail: { id: String(id) } }))
    } else app.error(data?.error || '切换失败')
  } catch (e) { app.error(e.response?.data?.error || '切换请求失败') }
}

/* ---------- 手动添加 code ---------- */
const addCode = ref(''); const addName = ref(''); const addPlatform = ref('qq'); const addBusy = ref(false)
const ADD_CHS = [{ v: 'qq', l: 'QQ' }, { v: 'wx', l: '微信' }] // 仅 QQ / 微信
async function addByCode() {
  if (!addCode.value.trim()) { app.error('请输入 code'); return }
  addBusy.value = true
  try {
    const { data } = await api.post('/api/accounts', { name: addName.value || '新账号', code: addCode.value.trim(), platform: addPlatform.value })
    if (data?.ok) { app.success(`添加成功（${addPlatform.value}）`); sheet.value = ''; addCode.value = ''; addName.value = ''; location.reload() }
    else app.error('添加失败: ' + (data?.error || '未知'))
  } catch (e) { app.error('请求失败: ' + (e.response?.data?.error || e.message)) } finally { addBusy.value = false }
}

/* ---------- 扫码登录 YYB ---------- */
const qrUrl = ref(''); const qrMsg = ref(''); const qrBusy = ref(false)
// 扫码渠道切换：wx = 微信（当前游戏网关链路仅支持微信区）；qq = 预留位，引导用手动 code 接入
const qrType = ref('wx')
const QR_CHS = [{ v: 'wx', l: '微信' }, { v: 'qq', l: 'QQ' }]
function switchQrType(t) {
  if (qrType.value === t) return
  qrType.value = t
  stopQr(); stopQqQr(); qrUrl.value = ''; qrMsg.value = ''
  if (t === 'wx') startQrLogin()
  else { loadQQConfig(); qqStartQr() }
}
let qrTimer = null
let qrSeq = 0
function stopQr() {
  qrSeq++
  if (qrTimer) { clearInterval(qrTimer); qrTimer = null }
}
function _qrStatus(text) { qrMsg.value = text }
function _renderQrImg(src) {
  if (!src) return ''
  if (/^data:/i.test(src) || /^https?:/i.test(src) || src.charAt(0) === '/') return src
  return 'data:image/png;base64,' + src
}
function _qrErr(e, fallback) {
  return e?.response?.data?.error || e?.message || fallback
}
async function _pollQrOnce(sessionId) {
  try {
    const { data } = await api.post('/api/yyb/qr/poll', { sessionId }, { timeout: 60000 })
    if (!(data?.ok && data.data)) return { terminal: true, msg: data?.error || '扫码状态异常' }
    const st = data.data.status
    if (st === 'authorized' || st === 'confirmed') return { terminal: false, ready: true }
    if (st === 'pending') { _qrStatus('等待手机扫码…'); return { terminal: false, ready: false } }
    if (st === 'scanned') { _qrStatus('已扫描，请在手机上确认…'); return { terminal: false, ready: false } }
    if (st === 'cancelled') return { terminal: true, msg: '已取消扫码' }
    if (st === 'expired') return { terminal: true, msg: '二维码已失效' }
    if (st === 'unknown') return { terminal: true, msg: '二维码状态未知，请重新获取' }
    return { terminal: false, ready: false }
  } catch (e) {
    const status = e?.response?.status
    const msg = _qrErr(e, '')
    if (status === 401) return { terminal: true, msg: '登录已过期，请重新登录' }
    if (status && status >= 400 && status < 500) return { terminal: true, msg: msg || '扫码失败' }
    return { terminal: false, ready: false, err: msg || '扫码轮询失败' }
  }
}
async function startQrLogin() {
  if (qrType.value !== 'wx') return // 仅微信走 YYB 扫码；QQ 走 NapCat 表单
  stopQr(); qrUrl.value = ''; qrMsg.value = '正在获取二维码…'; qrBusy.value = true
  const seq = qrSeq
  let sessionId = null
  try {
    // 1. 拉二维码
    const { data } = await api.post('/api/yyb/qr/create', { type: qrType.value }, { timeout: 60000 })
    if (seq !== qrSeq) return
    if (!(data?.ok && data?.data)) { _qrStatus('获取二维码失败: ' + (data?.error || '未知')); qrBusy.value = false; return }
    const d = data.data
    sessionId = d.session_id
    if (!sessionId) { _qrStatus('后端未返回 session_id'); qrBusy.value = false; return }
    qrUrl.value = _renderQrImg(d.image_base64 || d.image_url || '')
    _qrStatus('请使用手机微信扫描（此二维码为微信登录码，用 QQ 扫无效）')

    // 2. 轮询（顺序请求，上限 3 分钟）
    const deadline = Date.now() + 180000
    let ready = false, terminalMsg = null
    while (Date.now() < deadline) {
      if (seq !== qrSeq) return
      const pr = await _pollQrOnce(sessionId)
      if (seq !== qrSeq) return
      if (pr.terminal) { terminalMsg = pr.msg; break }
      if (pr.ready) { ready = true; break }
      if (pr.err) _qrStatus(pr.err)
      await new Promise(r => setTimeout(r, 2500))
    }
    if (seq !== qrSeq) return
    if (terminalMsg) { _qrStatus(terminalMsg); qrBusy.value = false; return }
    if (!ready) { _qrStatus('登录超时，请重新获取二维码'); qrBusy.value = false; return }

    // 3. confirm → openid
    _qrStatus('手机已确认，正在登录…')
    const cf = (await api.post('/api/yyb/qr/confirm', { sessionId }, { timeout: 60000 })).data
    if (seq !== qrSeq) return
    if (!(cf && cf.ok && cf.data)) { _qrStatus('确认登录失败: ' + ((cf && cf.error) || '未知')); qrBusy.value = false; return }
    const cfa = cf.data
    const openid = cfa.openid || cfa.OpenID || cfa.ref
    if (!openid) { _qrStatus('未获取到 openid'); qrBusy.value = false; return }

    // 4. getcode
    const gc = (await api.post('/api/yyb/getcode', { openid }, { timeout: 60000 })).data
    const code = gc && gc.data && gc.data.code
    if (!code) { _qrStatus('获取 code 失败: ' + ((gc && gc.error) || '未知')); qrBusy.value = false; return }

    // 5. 添加账号
    const platform = (cfa && cfa.platform) || 'wx'
    const name = (cfa && (cfa.nickname || cfa.alias || cfa.name)) || '新账号'
    const add = (await api.post('/api/accounts', { name, code, platform, openId: openid })).data
    qrBusy.value = false
    if (add.ok) {
      // 把扫码弹窗里的重连配置应用到新账号（默认开启/3分钟/3次，用户可在弹窗内先改）
      const newId = add.activeAccountId || (Array.isArray(add.data) && add.data.length && add.data[add.data.length - 1].id)
      if (newId) {
        // 将新账号设为当前选中账号，供首页/活动中心等使用
        setAccountId(newId)
        try {
          await api.post(`/api/reconnect/config?accountId=${encodeURIComponent(newId)}`, {
            enabled: rcfg.enabled,
            reconnectDelayMin: Math.max(1, Number(rcfg.delay) || 3),
            reconnectMaxAttempts: Math.max(1, Number(rcfg.max) || 3),
          })
        } catch (_) { /* rc 配置失败不影响账号添加 */ }
      }
      stopQr(); sheet.value = ''; app.success('扫码登录成功'); location.reload()
    }
    else { _qrStatus('添加账号失败: ' + (add.error || '未知')) }
  } catch (e) {
    qrBusy.value = false
    _qrStatus('扫码登录失败: ' + _qrErr(e, '未知错误'))
    console.error(e)
  }
}

/* ---------- 掉线自动重连（扫码弹窗内 rc-panel） ---------- */
const rcfg = reactive({ enabled: true, delay: 3, max: 3 })
const rcState = ref('连接：-')
function _fmtRc(st) {
  const s = (st && st.state) || '-'
  let t = '连接：' + s
  if (st && st.stopped) t += '（已停止）'
  if (st && st.attempts) t += ' · 已重连' + st.attempts + '次'
  return t
}
async function loadRc(accountId) {
  try {
    const { data } = await api.get('/api/reconnect/config', { params: accountId ? { accountId } : {} })
    const d = data && data.data
    if (!d) return
    rcfg.enabled = !!d.enabled
    if (d.reconnectDelayMin !== undefined) rcfg.delay = d.reconnectDelayMin
    if (d.reconnectMaxAttempts !== undefined) rcfg.max = d.reconnectMaxAttempts
    if (d.state) rcState.value = _fmtRc(d.state)
  } catch (e) {}
}
async function saveRc(accountId) {
  try {
    const url = accountId ? `/api/reconnect/config?accountId=${encodeURIComponent(accountId)}` : '/api/reconnect/config'
    const { data } = await api.post(url, { enabled: rcfg.enabled, reconnectDelayMin: Number(rcfg.delay) || 3, reconnectMaxAttempts: Number(rcfg.max) || 3 })
    if (data?.ok) { app.success('设置已保存'); await loadRc(accountId) }
    else app.error('保存失败：' + (data?.error || '未知'))
  } catch (e) { app.error('保存失败') }
}
const rcRetryBusy = ref(false)
async function retryRc(accountId) {
  rcRetryBusy.value = true
  try {
    const url = accountId ? `/api/reconnect/retry?accountId=${encodeURIComponent(accountId)}` : '/api/reconnect/retry'
    const { data } = await api.post(url, {})
    if (data?.ok) { app.success('已触发重连，将在后台执行'); setTimeout(() => loadRc(accountId), 2000) }
    else app.error('触发失败' + (data?.error ? '：' + data.error : ''))
  } catch (e) { app.error('触发失败') } finally { rcRetryBusy.value = false }
}

/* ---------- 编辑 / 删除 ---------- */
async function saveEdit() {
  if (!editing.value) return
  try {
    const { data } = await api.put(`/api/accounts/${encodeURIComponent(editing.value.id)}`, { name: editing.value.name || '', code: editing.value.code || '', platform: '' })
    if (data?.ok) { app.success(data.relinked ? '已保存并刷新登录凭证' : '已保存'); sheet.value = ''; editing.value = null; await loadAccounts() }
    else app.error('保存失败: ' + (data?.error || '?'))
  } catch (e) { app.error(e.response?.data?.error || '保存失败') }
}
async function delAcc(id) {
  delTarget.value = id
}
async function confirmDel() {
  const id = delTarget.value
  if (!id) return
  delTarget.value = null
  try {
    const { data } = await api.delete(`/api/accounts/${encodeURIComponent(id)}`)
    if (data?.ok) {
      app.success('已删除')
      if (String(activeId.value) === String(id)) {
        account.switchAccount('')
      }
      await loadAccounts()
    }
    else app.error('删除失败: ' + (data?.error || '?'))
  } catch (e) { app.error(e.response?.data?.error || '删除失败') }
}

/* ---------- 退出登录 ---------- */
function logout() {
  // 统一清 admin_token + 内存登录态（account.logout 内部 setToken('') 会 removeItem，并）
  account.logout()
  router.push('/login')
}

/* ---------- 第三方应用宝登录（与内置 YYB 互不冲突） ---------- */
const t3rdApiBase = ref(''); const t3rdApiToken = ref(''); const t3rdOpenid = ref(''); const t3rdName = ref('')
const t3rdAuto = ref(true); const t3rdDelay = ref(5); const t3rdMax = ref(3)
const t3rdBusy = ref(false); const t3rdErr = ref('')
async function addByThirdpartyYyb() {
  t3rdErr.value = ''
  if (!t3rdApiBase.value.trim() || !t3rdOpenid.value.trim() || !t3rdApiToken.value.trim()) {
    t3rdErr.value = '请填写接口地址、APITOKEN、OPENID'
    return
  }
  t3rdBusy.value = true
  try {
    // 1. 向第三方接口换登录 code
    const tc = (await api.post('/api/yyb/thirdparty-code', {
      apiBase: t3rdApiBase.value.trim(),
      apiToken: t3rdApiToken.value.trim(),
      openid: t3rdOpenid.value.trim(),
    })).data
    if (!(tc?.ok && tc?.data?.code)) {
      t3rdErr.value = tc?.error || '获取登录 code 失败'
      t3rdBusy.value = false
      return
    }
    const code = tc.data.code
    const openid = (tc.data.openid || t3rdOpenid.value).trim()
    const name = t3rdName.value.trim() || `第三方应用宝${openid.slice(-4)}`

    // 2. 添加账号（thirdparty 字段一并持久化，重连由 refreshCodeFromYyb 第三方分支负责）
    const add = (await api.post('/api/accounts', {
      name, code, platform: 'wx', openId: openid,
      thirdparty: {
        apiBase: t3rdApiBase.value.trim(),
        apiToken: t3rdApiToken.value.trim(),
        openid,
        autoReconnect: t3rdAuto.value === true,
        reconnectDelayMin: Math.max(1, Number(t3rdDelay.value) || 5),
        reconnectMaxAttempts: Math.max(1, Number(t3rdMax.value) || 3),
      },
    })).data
    if (!add?.ok) {
      t3rdErr.value = '添加账号失败: ' + (add?.error || '未知')
      t3rdBusy.value = false
      return
    }
    const newId = add.activeAccountId || (Array.isArray(add.data) && add.data.length && add.data[add.data.length - 1].id)
    // 3. 把第三方专属重连配置同步到账号级 AutoReconnect（即使勾选不勾都写，留档可追踪）
    if (newId) {
      try {
        await api.post(`/api/reconnect/config?accountId=${encodeURIComponent(newId)}`, {
          enabled: t3rdAuto.value === true,
          reconnectDelayMin: Math.max(1, Number(t3rdDelay.value) || 5),
          reconnectMaxAttempts: Math.max(1, Number(t3rdMax.value) || 3),
        })
      } catch (_) { /* rc 配置失败不影响账号添加 */ }
    }
    app.success('第三方应用宝登录成功')
    sheet.value = ''
    t3rdApiBase.value = ''; t3rdApiToken.value = ''; t3rdOpenid.value = ''; t3rdName.value = ''
    location.reload()
  } catch (e) {
    t3rdErr.value = e.response?.data?.error || e.message || '第三方登录失败'
  } finally { t3rdBusy.value = false }
}

/* ---------- QQ 扫码登录（NapCat 桥接：WebUI 二维码 + qq-code 插件取 code） ---------- */
const qqNapcatBase = ref(''); const qqWebuiToken = ref(''); const qqNapcatKey = ref(''); const qqAppId = ref('1112386029')
const qqVer = ref(''); const qqName = ref(''); const qqSaveDefault = ref(false); const qqBusy = ref(false); const qqMsg = ref('')
const qqState = ref('') // '' | loading | qr | need_config | scanned | success | expired | error
const qqQrImg = ref(''); const qqQrSession = ref(''); const showQqAdv = ref(false)
async function loadQQConfig() {
  try {
    const { data } = await api.get('/api/qq/config')
    const d = data && data.data
    if (!d) return
    if (d.napcatBase) qqNapcatBase.value = d.napcatBase
    if (d.appId) qqAppId.value = d.appId
    if (d.ver) qqVer.value = d.ver
  } catch (e) {}
}
function _qqCredsPayload() {
  return {
    napcatBase: qqNapcatBase.value.trim(),
    webuiToken: qqWebuiToken.value,
    napcatKey: qqNapcatKey.value,
    appId: qqAppId.value.trim() || '1112386029',
    ver: qqVer.value.trim(),
    name: qqName.value.trim(),
  }
}
async function _qqSaveDefault() {
  if (!qqSaveDefault.value) return
  try {
    await api.post('/api/qq/config', {
      enabled: true,
      napcatBase: qqNapcatBase.value.trim(),
      napcatKey: qqNapcatKey.value,
      webuiToken: qqWebuiToken.value,
      appId: qqAppId.value.trim() || '1112386029',
      ver: qqVer.value.trim(),
    })
  } catch (_) { /* 保存失败不影响本次登录 */ }
}
// 登录成功收尾：切当前账号、套用重连配置、刷新页面
async function _qqFinish(d) {
  qqState.value = 'success'
  qqMsg.value = '登录成功，正在进入农场…'
  stopQqQr(false)
  const newId = d && d.activeAccountId
  if (newId) {
    setAccountId(newId)
    try {
      await api.post(`/api/reconnect/config?accountId=${encodeURIComponent(newId)}`, {
        enabled: rcfg.enabled,
        reconnectDelayMin: Math.max(1, Number(rcfg.delay) || 3),
        reconnectMaxAttempts: Math.max(1, Number(rcfg.max) || 3),
      })
    } catch (_) { /* rc 配置失败不影响登录 */ }
  }
  setTimeout(() => { sheet.value = ''; app.success('QQ 扫码登录成功'); location.reload() }, 900)
}
let qqSeq = 0
function stopQqQr(cancelRemote = true) {
  qqSeq++
  const sid = qqQrSession.value
  qqQrSession.value = ''
  if (sid && cancelRemote) { api.post('/api/qq/qr/cancel', { sessionId: sid }).catch(() => {}) }
}
// 开始 QQ 扫码：后端自动判断「已登录直接登录 / 需要扫码 / 需要配置」
async function qqStartQr() {
  stopQqQr()
  qqMsg.value = ''; qqQrImg.value = ''
  qqState.value = 'loading'
  const seq = qqSeq
  try {
    const { data } = await api.post('/api/qq/qr/start', _qqCredsPayload(), { timeout: 60000 })
    if (seq !== qqSeq) return
    if (!(data?.ok && data?.data)) { qqState.value = 'error'; qqMsg.value = data?.error || '启动失败'; return }
    const d = data.data
    if (d.state === 'need_config') {
      qqState.value = 'need_config'; loadQQConfig()
      if (!qqNapcatBase.value.trim() && d.triedBase) qqNapcatBase.value = d.triedBase
      qqMsg.value = d.hint || d.error || ''
      return
    }
    if (d.state === 'success') { await _qqFinish(d); return }
    if (d.state === 'error') {
      qqState.value = d.needConfig ? 'need_config' : 'error'
      qqMsg.value = d.error || '未知错误'
      return
    }
    if (d.state === 'qr') {
      qqState.value = 'qr'
      qqQrImg.value = d.qrImage || ''
      qqQrSession.value = d.sessionId || ''
      qqMsg.value = '请使用手机 QQ「扫一扫」扫描二维码'
      _qqPoll()
    }
  } catch (e) {
    qqState.value = 'error'
    qqMsg.value = 'QQ 扫码启动失败: ' + _qrErr(e, '未知错误')
  }
}
// 轮询扫码状态（后端在 NapCat 登录成功后自动建号连网关）
async function _qqPoll() {
  const seq = qqSeq
  const sid = qqQrSession.value
  if (!sid) return
  const deadline = Date.now() + 180000
  while (Date.now() < deadline) {
    if (seq !== qqSeq) return
    try {
      const { data } = await api.get('/api/qq/qr/status', { params: { id: sid }, timeout: 30000 })
      if (seq !== qqSeq) return
      if (data?.ok && data?.state) {
        const st = data.state
        if (st === 'waiting') { qqState.value = 'qr'; if (data.qrImage) qqQrImg.value = data.qrImage }
        else if (st === 'scanned') { qqState.value = 'scanned'; qqMsg.value = '已扫描，请在手机上确认登录…' }
        else if (st === 'success') { await _qqFinish(data); return }
        else if (st === 'expired') { qqState.value = 'expired'; qqMsg.value = '二维码已失效'; return }
        else if (st === 'error') { qqState.value = 'error'; qqMsg.value = data.error || data.hint || '登录失败'; return }
      }
    } catch (e) {
      if (e?.response?.status === 404) { qqState.value = 'error'; qqMsg.value = '会话已过期，请重新开始'; return }
    }
    await new Promise(r => setTimeout(r, 2000))
  }
  if (seq === qqSeq) { qqState.value = 'error'; qqMsg.value = '登录超时，请重新开始' }
}
// 刷新二维码（失效后）
async function qqRefreshQr() {
  const sid = qqQrSession.value
  if (!sid) { qqStartQr(); return }
  qqMsg.value = '正在刷新二维码…'
  try {
    const { data } = await api.post('/api/qq/qr/refresh', { sessionId: sid }, { timeout: 30000 })
    if (data?.ok && data?.data?.qrImage) {
      qqQrImg.value = data.data.qrImage; qqState.value = 'qr'
      qqMsg.value = '二维码已刷新，请重新扫描'
      _qqPoll()
    } else { qqMsg.value = '刷新失败: ' + (data?.error || '未知') }
  } catch (e) { qqMsg.value = '刷新失败: ' + _qrErr(e, '未知') }
}
// 保存配置（可选）并生成二维码
async function qqSaveCfgAndStart() {
  qqMsg.value = ''
  if (!qqNapcatBase.value.trim()) { qqMsg.value = '请填写 NapCat 地址'; return }
  await _qqSaveDefault()
  qqStartQr()
}
// 直接登录（NapCat 已登录、跳过扫码）
async function qqDirectLogin() {
  qqMsg.value = ''
  if (!qqNapcatBase.value.trim()) { qqMsg.value = '请填写 NapCat 地址'; return }
  qqBusy.value = true
  try {
    const { data } = await api.post('/api/qq/login', _qqCredsPayload(), { timeout: 60000 })
    if (!(data && data.ok)) { qqMsg.value = 'QQ 登录失败: ' + (data?.error || '未知'); return }
    await _qqFinish(data)
  } catch (e) {
    qqMsg.value = 'QQ 登录失败: ' + _qrErr(e, '未知')
  } finally { qqBusy.value = false }
}

// 切号事件：刷新账号列表（在线状态/active 标记随新账号变化）
const onAccountSwitched = () => loadAccounts()
onMounted(() => { loadAccounts(); window.addEventListener('account-switched', onAccountSwitched) })
onUnmounted(() => { stopQr(); stopQqQr(); window.removeEventListener('account-switched', onAccountSwitched) })
</script>

<template>
  <div>
    <h3 style="font-size:20px;font-weight:700;margin:2px 2px 0">账号</h3>

    <div class="sec-title"><span>已登录账号</span><span class="link" @click="sheet='manage'">管理</span></div>
    <div class="acc-list">
      <button v-for="a in accounts" :key="a.id" class="acc-row" :class="{ active: String(a.id) === String(activeId) }" @click="switchAcc(a.id)">
        <div class="a-av">🐰</div>
        <div class="a-info"><b>{{ a.name || '未命名' }}</b><span>{{ a.platform || 'qq' }} · {{ ({ online: '在线', offline: '离线' })[a.status] || a.status || '离线' }}</span></div>
        <span class="check">{{ String(a.id) === String(activeId) ? '✓' : '' }}</span>
      </button>
      <p v-if="!accounts.length" style="font-size:12px;color:var(--muted);text-align:center;padding:16px 0">暂无账号</p>
    </div>

    <div class="sec-title"><span>添加账号</span><span style="font-size:12px;color:var(--muted)">{{ accounts.length }} / {{ limitText }}</span></div>
    <div class="menu">
      <button class="sub-item" @click="sheet='add'"><span class="mi">🔑</span>手动添加 code<span class="arr">›</span></button>
      <button class="sub-item" @click="sheet='qr'; loadRc(); loadQQConfig(); qrType='wx'; startQrLogin()"><span class="mi">📱</span>扫码登录<span class="arr">›</span></button>
      <button class="sub-item" @click="sheet='thirdparty'; t3rdErr=''"><span class="mi">🔗</span>第三方登录<span class="arr">›</span></button>
    </div>

    <button class="logout" @click="logout">退出登录</button>
  </div>

  <!-- 账号管理 sheet -->
  <div v-if="sheet==='manage'" class="sheet-mask show" @click="sheet=''"></div>
  <div v-if="sheet==='manage'" class="sheet show">
    <div class="handle"></div>
    <h3>⚙️ 账号管理</h3>
    <div style="max-height:60vh;overflow:auto">
      <div v-for="a in accounts" :key="a.id" class="acc-row" :class="{ active: String(a.id) === String(activeId) }" style="flex-wrap:wrap">
        <div class="a-info"><b>{{ a.name }}</b><span>{{ a.platform }} · {{ a.status }}</span></div>
        <div style="display:flex;gap:6px;margin-left:auto">
          <button class="bi-use" @click="switchAcc(a.id)">切换</button>
          <button class="bi-use" @click="editing = { id: a.id, name: a.name, code: a.code || '' }; loadRc(a.id)">编辑</button>
          <button class="bi-sell" @click.stop="delAcc(a.id)">删除</button>
        </div>
        <div v-if="editing && editing.id === a.id" style="width:100%;margin-top:8px">
          <div style="display:flex;gap:6px">
            <input v-model="editing.name" class="field" placeholder="备注名" style="flex:1">
            <input v-model="editing.code" class="field" placeholder="code（留空不变）" style="flex:2">
            <button class="bi-use" @click="saveEdit">保存</button>
          </div>
          <!-- 掉线自动重连（账号级，登录后也可改） -->
          <div class="rc-panel" style="margin-top:10px">
            <div class="rc-head">📡 掉线自动重连 <small>该账号断线后延迟换 code 自动重连</small></div>
            <div class="rc-row">
              <span>开启自动重连</span>
              <div class="switch" :class="{ on: rcfg.enabled }" @click="rcfg.enabled = !rcfg.enabled"></div>
            </div>
            <div class="rc-grid">
              <div class="rc-col">
                <label class="rc-label">离线多久重连</label>
                <div class="sec-field">
                  <input type="number" v-model.number="rcfg.delay" class="field" min="1">
                  <span class="unit">分钟</span>
                </div>
              </div>
              <div class="rc-col">
                <label class="rc-label">失败几次停止</label>
                <div class="sec-field">
                  <input type="number" v-model.number="rcfg.max" class="field" min="1">
                  <span class="unit">次</span>
                </div>
              </div>
            </div>
            <div class="rc-foot">
              <span class="rc-state">{{ rcState }}</span>
              <div class="rc-btns">
                <button class="chip" :disabled="rcRetryBusy" @click="retryRc(editing.id)">{{ rcRetryBusy ? '重连中…' : '🔁 立即重连' }}</button>
                <button class="chip" @click="saveRc(editing.id)">💾 保存</button>
              </div>
            </div>
          </div>
        </div>
      </div>
      <p v-if="!accounts.length" style="text-align:center;color:var(--muted);padding:20px 0">暂无账号</p>
    </div>
    <button class="close" style="margin-top:16px" @click="sheet=''">关闭</button>
  </div>

  <div v-if="delTarget" class="sheet-mask show" @click="delTarget = null"></div>
  <div v-if="delTarget" class="sheet show">
    <div class="handle"></div>
    <h3>删除账号</h3>
    <p class="sub">确定删除该账号？删除后需重新添加 code 才能登录。</p>
    <div style="display:flex;gap:8px;margin-top:16px">
      <button class="close" style="flex:1;margin:0" @click="delTarget = null">取消</button>
      <button class="close" style="flex:1;margin:0;background:var(--danger,#e5484d);color:#fff;border-color:transparent" @click="confirmDel">确认删除</button>
    </div>
  </div>

  <!-- 手动添加 code sheet -->
  <div v-if="sheet==='add'" class="sheet-mask show" @click="sheet=''"></div>
  <div v-if="sheet==='add'" class="sheet show">
    <div class="handle"></div>
    <h3>🔑 手动添加 code</h3>
    <input v-model="addName" class="field" placeholder="备注名（可选）" style="margin-top:12px">
    <input v-model="addCode" class="field" placeholder="粘贴 code（可从登录页抓取）" style="margin-top:8px">
    <div style="display:flex;gap:8px;margin-top:8px">
      <button v-for="ch in ADD_CHS" :key="ch.v" class="chip" :class="{ on: addPlatform === ch.v }" @click="addPlatform = ch.v">{{ ch.l }}</button>
    </div>
    <button class="close" :disabled="addBusy" style="margin-top:16px" @click="addByCode">{{ addBusy ? '提交中…' : '添加账号' }}</button>
  </div>

  <!-- 扫码登录 sheet -->
  <div v-if="sheet==='qr'" class="sheet-mask show" @click="sheet=''; stopQr(); stopQqQr()"></div>
  <div v-if="sheet==='qr'" class="sheet show">
    <div class="handle"></div>
    <h3>📱 扫码登录</h3>
    <div style="display:flex;flex-direction:column;align-items:center;padding:16px 0;width:100%;max-height:calc(85dvh - 170px);overflow-y:auto;overflow-x:hidden">
      <!-- 微信 / QQ 渠道切换 -->
      <div style="display:flex;gap:8px;margin-bottom:12px">
        <button v-for="ch in QR_CHS" :key="ch.v" class="chip" :class="{ on: qrType === ch.v }" @click="switchQrType(ch.v)">{{ ch.l }}</button>
      </div>

      <!-- 微信扫码（当前唯一可用的扫码链路） -->
      <template v-if="qrType === 'wx'">
        <img v-if="qrUrl" :src="qrUrl" style="width:150px;height:150px;border-radius:12px;background:#fff;object-fit:contain">
        <p v-else-if="qrBusy" style="color:var(--muted)">获取二维码中…</p>
        <p v-else style="color:var(--muted)">点击下方按钮获取二维码</p>
        <p v-if="qrMsg" style="font-size:12px;color:var(--muted);margin-top:8px;text-align:center">{{ qrMsg }}</p>
        <p style="font-size:11px;color:var(--muted);margin-top:6px;text-align:center;max-width:280px;line-height:1.6">
          提示：若手机提示「登录地异常」，为微信安全风控（二维码由服务器 IP 生成，与手机所在地不一致），在手机上点「仍要继续 / 确认登录」即可；若被要求强制验证，请换常用网络环境重试。
        </p>
      </template>

      <!-- QQ 渠道：NapCat WebUI 二维码 + qq-code 插件取 code（参考 fubiwangluo/qq-code） -->
      <template v-else>
        <!-- 未配置 / 出错：显示配置表单 -->
        <template v-if="qqState==='need_config' || qqState==='error'">
          <div style="width:100%;max-width:340px;display:flex;flex-direction:column;gap:8px">
            <p style="font-size:12px;color:var(--muted);text-align:center;line-height:1.6;margin:0">
              QQ 二维码由 <b>NapCat</b> 生成（手机 QQ 扫码授权登录后，自动签出农场 code）。<br>
              推荐<b>与本项目同机部署</b>（无需额外设备）：在服务器上执行仓库自带的一键脚本
              <code>deploy/napcat/setup.sh</code>，完成后直接点「生成二维码」即可。
            </p>
            <input v-model="qqNapcatBase" class="field" placeholder="NapCat 地址，如 http://127.0.0.1:6099">
            <input v-model="qqWebuiToken" class="field" placeholder="WebUI Token（NapCat webui.json 里的 token）" type="password" autocomplete="off">
            <input v-model="qqNapcatKey" class="field" placeholder="插件 Key（可选，data/config.json 的 key）" type="password" autocomplete="off">
            <template v-if="showQqAdv">
              <div style="display:flex;gap:8px">
                <input v-model="qqAppId" class="field" placeholder="appId（默认 1112386029）" style="flex:2">
                <input v-model="qqVer" class="field" placeholder="版本(可选)" style="flex:1.4">
              </div>
              <input v-model="qqName" class="field" placeholder="备注名（可选，留空用 QQ 号）">
            </template>
            <label style="display:flex;align-items:center;gap:6px;font-size:12px;color:var(--muted);cursor:pointer">
              <input type="checkbox" v-model="qqSaveDefault"> 保存为默认（下次免填）
            </label>
            <p v-if="qqMsg" style="font-size:12px;color:var(--danger,#e5484d);text-align:center;margin:0;word-break:break-all">{{ qqMsg }}</p>
            <button class="close" :disabled="qqBusy" style="margin-top:4px" @click="qqSaveCfgAndStart">{{ qqState==='loading' ? '连接中…' : '生成二维码' }}</button>
            <button class="chip" style="align-self:center" @click="showQqAdv = !showQqAdv">{{ showQqAdv ? '收起高级选项' : '高级选项' }}</button>
            <p style="font-size:11px;color:var(--muted);text-align:center;margin:0;line-height:1.6">
              WebUI Token 在 NapCat 的 <code>webui.json</code> 或启动日志中查看。<br>
              若 NapCat 已登录 QQ，点「生成二维码」后会跳过扫码直接登录。
            </p>
          </div>
        </template>

        <!-- 二维码展示 / 登录状态 -->
        <template v-else>
          <div style="position:relative;width:190px;height:190px;display:flex;align-items:center;justify-content:center;border-radius:14px;background:#fff;overflow:hidden;flex-shrink:0">
            <img v-if="qqQrImg" :src="qqQrImg" alt="QQ 登录二维码" style="width:170px;height:170px;object-fit:contain">
            <p v-else style="color:#888;font-size:12px;margin:0">{{ qqState==='loading' ? '连接 NapCat 中…' : '等待二维码…' }}</p>
            <div v-if="qqState==='expired'" class="qr-overlay" @click="qqRefreshQr"><span>二维码已失效<br>点击刷新</span></div>
            <div v-if="qqState==='success'" class="qr-overlay ok"><span style="font-size:30px">✓</span></div>
          </div>
          <p style="font-size:12px;color:var(--muted);margin:10px 0 0;text-align:center">
            <template v-if="qqState==='loading'">正在连接 NapCat…</template>
            <template v-else-if="qqState==='scanned'">已扫描，请在手机上确认登录…</template>
            <template v-else-if="qqState==='success'">登录成功，正在进入农场…</template>
            <template v-else-if="qqState==='expired'">二维码已失效，点击图片刷新</template>
            <template v-else>请使用手机 QQ「扫一扫」扫描二维码</template>
          </p>
          <p v-if="qqState==='error' && qqMsg" style="font-size:12px;color:var(--danger,#e5484d);margin:6px 0 0;text-align:center;max-width:320px;word-break:break-all">{{ qqMsg }}</p>
          <button v-if="qqState==='expired'" class="close" style="margin-top:8px" @click="qqRefreshQr">刷新二维码</button>
          <p style="font-size:11px;color:var(--muted);margin-top:8px;text-align:center;max-width:300px;line-height:1.6">
            扫码将在 NapCat 上授权登录该 QQ，登录后自动通过 <code>loginWithAppId</code> 签出农场 code 并加入账号列表；被踢后自动重签重连。<br>
            若 NapCat 已登录目标 QQ，可 <span class="link" style="cursor:pointer" @click="qqDirectLogin">直接登录（跳过扫码）</span>
          </p>
        </template>
      </template>
    </div>

    <!-- 掉线自动重连 -->
    <div class="rc-panel" style="margin-top:14px">
      <div class="rc-head">📡 掉线自动重连 <small>断线后延迟换 code 自动重连</small></div>
      <div class="rc-row">
        <span>开启自动重连</span>
        <div class="switch" :class="{ on: rcfg.enabled }" @click="rcfg.enabled = !rcfg.enabled"></div>
      </div>
      <div class="rc-grid">
        <div class="rc-col">
          <label class="rc-label">离线多久重连</label>
          <div class="sec-field">
            <input type="number" v-model.number="rcfg.delay" class="field" min="1">
            <span class="unit">分钟</span>
          </div>
        </div>
        <div class="rc-col">
          <label class="rc-label">失败几次停止</label>
          <div class="sec-field">
            <input type="number" v-model.number="rcfg.max" class="field" min="1">
            <span class="unit">次</span>
          </div>
        </div>
      </div>
      <div class="rc-foot">
        <span class="rc-state">{{ rcState }}</span>
        <div class="rc-btns">
          <button class="chip" :disabled="rcRetryBusy" @click="retryRc">{{ rcRetryBusy ? '重连中…' : '🔁 立即重连' }}</button>
          <button class="chip" @click="saveRc">💾 保存</button>
        </div>
      </div>
    </div>

    <button class="close" v-if="!qrBusy && qrType==='wx'" style="margin-top:4px" @click="startQrLogin">重新获取二维码</button>
    <button class="close" style="margin-top:8px" @click="sheet=''; stopQr(); stopQqQr()">关闭</button>
  </div>

  <!-- 第三方应用宝登录 sheet（与内置 YYB 互不冲突；自带账号级独立重连配置） -->
  <div v-if="sheet==='thirdparty'" class="sheet-mask show" @click="sheet=''"></div>
  <div v-if="sheet==='thirdparty'" class="sheet show">
    <div class="handle"></div>
    <h3>🔗 第三方应用宝登录</h3>
    <p style="font-size:12px;color:var(--muted);margin-top:8px;line-height:1.5">
      填入第三方 <b>YYB 接口地址、APITOKEN、OPENID</b> 即可获取登录 code 并添加账号。被踢/异地登录后将按下方设置自动重连（与内置 YYB 互不影响）。
    </p>
    <input v-model="t3rdApiBase" class="field" placeholder="接口地址，例如 http://211.154.25.123:28999" style="margin-top:12px">
    <input v-model="t3rdApiToken" class="field" placeholder="第三方接口 APITOKEN" style="margin-top:8px" type="password" autocomplete="off">
    <input v-model="t3rdOpenid" class="field" placeholder="第三方账号 openid" style="margin-top:8px">
    <input v-model="t3rdName" class="field" placeholder="账号备注（可选，留空则用 openid 后四位）" style="margin-top:8px">

    <div class="rc-panel" style="margin-top:14px">
      <div class="rc-head">📡 离线自动重连<small>账号级独立配置，与内置 YYB 不冲突</small></div>
      <div class="rc-row">
        <span>启用离线自动重连</span>
        <div class="switch" :class="{ on: t3rdAuto }" @click="t3rdAuto = !t3rdAuto"></div>
      </div>
      <div class="rc-grid">
        <div class="rc-col">
          <label class="rc-label">离线几分钟后重连</label>
          <div class="sec-field">
            <input type="number" v-model.number="t3rdDelay" class="field" min="1">
            <span class="unit">分钟</span>
          </div>
        </div>
        <div class="rc-col">
          <label class="rc-label">失败几次后停止</label>
          <div class="sec-field">
            <input type="number" v-model.number="t3rdMax" class="field" min="1">
            <span class="unit">次</span>
          </div>
        </div>
      </div>
    </div>

    <p v-if="t3rdErr" style="font-size:12px;color:var(--danger,#e5484d);margin-top:10px">{{ t3rdErr }}</p>

    <button class="close" :disabled="t3rdBusy" style="margin-top:16px" @click="addByThirdpartyYyb">{{ t3rdBusy ? '添加并登录中…' : '添加并登录' }}</button>
  </div>
</template>

<style scoped>
.bi-use,
.bi-sell {
  border: 1px solid var(--border); background: var(--card-strong); color: var(--foreground);
  border-radius: 6px; font-size: 11px; line-height: 1; font-weight: 700; cursor: pointer; padding: 6px 9px;
}
.bi-use:active, .bi-sell:active { transform: scale(.95); }
.bi-sell { color: var(--danger, #e5484d); border-color: color-mix(in oklch, var(--danger, #e5484d) 40%, transparent); }
.qr-overlay {
  position: absolute; inset: 0; background: rgba(255, 255, 255, .94);
  display: flex; align-items: center; justify-content: center; text-align: center;
  font-size: 13px; color: var(--danger, #e5484d); cursor: pointer; line-height: 1.7; z-index: 1;
}
.qr-overlay.ok { color: #2e9e5b; cursor: default; font-size: 13px; }
</style>
