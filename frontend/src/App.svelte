<script>
  import { onMount } from 'svelte'
  import Access from './Access.svelte'
  import Deployments from './Deployments.svelte'
  import Repository from './Repository.svelte'
  import { api, repository } from './api.js'

  const sections = [
    { id: 'deployments', label: '部署记录', icon: '◈' },
    { id: 'commits', label: '提交', icon: '⎇' },
    { id: 'pulls', label: 'Pull Requests', icon: '⇆' },
    { id: 'actions', label: 'Actions', icon: '▷' },
    { id: 'security', label: '安全告警', icon: '♢' },
    { id: 'access', label: '访问权限', icon: '♙' },
  ]
  const loginFailed = new URLSearchParams(location.search).get('login') === 'failed'
  function sectionFromHash() {
    const route = location.hash.slice(1)
    return sections.find(({ id }) => id === route)?.id ?? 'deployments'
  }

  let section = $state(sectionFromHash())
  let user = $state(null)
  let loading = $state(true)
  let error = $state('')
  let login = $state({})
  let busy = $state(false)
  let brandIcon = $state('/brand-icon')
  let availableSections = $derived(user?.admin ? sections : sections.slice(0, 1))
  let active = $derived(availableSections.find(({ id }) => section === id) ?? availableSections[0])

  async function loadSession() {
    try {
      user = await api('/api/session')
    } catch (err) {
      if (err.status !== 401) error = err.message
      await loadLogin()
    } finally {
      loading = false
    }
  }

  onMount(() => {
    void loadSession()
    const timer = setInterval(() => {
      if (document.hidden) return
      brandIcon = '/brand-icon?v=' + Math.floor(Date.now() / 30000)
      document.querySelector('link[rel="icon"]').href = brandIcon
    }, 30000)
    return () => clearInterval(timer)
  })

  async function loadLogin() {
    try {
      login = await api('/api/login')
    } catch (err) {
      error = err.message
    }
  }

  async function pair() {
    busy = true
    error = ''
    try {
      if (!login.pairingURL) {
        login = await api('/auth/pairing', { method: 'POST' })
      } else {
        const result = await api('/auth/pairing/finish', { method: 'POST' })
        if (result.approved) location.replace('/')
        else error = '请在 PhotoLib 输入配对码并批准后，再检查结果。'
      }
    } catch (err) {
      error = err.message
      if (err.status === 401) await loadLogin()
    } finally {
      busy = false
    }
  }

  async function signInWithEmergencyPassword(event) {
    event.preventDefault()
    if (busy) return
    const form = event.currentTarget
    const password = new FormData(form).get('password')
    form.reset()
    busy = true
    error = ''
    try {
      await api('/auth/emergency', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ password }),
      })
      location.replace('/')
    } catch (err) {
      error = err.message
    } finally {
      busy = false
    }
  }

  function expire() {
    user = null
    error = '会话已过期，请重新登录。'
    void loadLogin()
  }

  async function logout() {
    try {
      await api('/auth/logout', { method: 'POST' })
      location.replace('/')
    } catch (err) {
      if (err.status === 401) expire()
      else error = err.message
    }
  }
</script>

<svelte:window onhashchange={() => section = sectionFromHash()} />

{#if loading || !user}
  <main class="login">
    <section class="login-card">
      <div class="brand"><img class="mark" src={brandIcon} width="34" height="34" alt="" /><span>Deplyt<small>自动化部署平台</small></span></div>
      <h1>PhotoLib 部署控制台</h1>
      {#if loading}
        <p role="status">正在检查登录状态…</p>
      {:else}
        {#if login.pairingURL}
          <p>在 PhotoLib 的批准页输入配对码 <code>{login.code}</code>。</p>
        {:else}
          <p>使用你的 PhotoLib 账号登录。</p>
        {/if}
        {#if error || loginFailed}
          <p class="notice danger" role="alert">{error || '授权未完成，请重新发起登录。'}</p>
        {/if}
        <div class="login-actions">
          {#if login.pairingURL}
            <a class="button primary" href={login.pairingURL} target="_blank" rel="noreferrer">前往 PhotoLib</a>
            <button class="button" disabled={busy} onclick={pair}>检查批准结果</button>
          {:else}
            <button class="button primary" disabled={busy} onclick={pair}>使用 PhotoLib 登录</button>
            {#if login.sso}<a class="button" href="/auth/start">管理员单点登录</a>{/if}
          {/if}
        </div>
        {#if login.emergency}
          <details class="emergency-login">
            <summary>使用应急密码登录</summary>
            <form method="post" action="/auth/emergency" onsubmit={signInWithEmergencyPassword}>
              <label>
                应急密码
                <input type="password" name="password" autocomplete="current-password" required />
              </label>
              <button type="submit" class="button" disabled={busy}>登录</button>
            </form>
          </details>
        {/if}
      {/if}
    </section>
  </main>
{:else}
  <div class="layout">
    <aside class="sidebar">
      <a class="brand" href="#deployments"><img class="mark" src={brandIcon} width="34" height="34" alt="" /><span>Deplyt<small>自动化部署平台</small></span></a>
      <nav aria-label="控制台">
        {#each availableSections as item (item.id)}
          <a href={'#' + item.id} aria-current={active.id === item.id ? 'page' : undefined}>
            <span class="nav-icon" aria-hidden="true">{item.icon}</span>{item.label}
          </a>
        {/each}
      </nav>
      <a class="repository-link" href={repository} target="_blank" rel="noreferrer">GitHub 仓库 ↗</a>
    </aside>
    <main class="workspace">
      <header class="topbar">
        <span>部署控制台 <span class="muted">/</span> <strong>PhotoLib</strong></span>
        <div class="account"><span>{user.username}</span><button class="button compact" onclick={logout}>退出</button></div>
      </header>
      <section class="content" aria-labelledby="section-title">
        <div class="page-heading">
          <h1 id="section-title">{user.allowed ? active.label : '等待访问授权'}</h1>
        </div>
        {#if error}<p class="notice danger" role="alert">{error}</p>{/if}
        {#if !user.allowed}
          <p>已识别你的 PhotoLib 账号。管理员授权后，刷新此页即可访问。</p>
          <button class="button" onclick={() => location.reload()}>刷新授权状态</button>
        {:else if active.id === 'deployments'}
          <Deployments {user} onExpired={expire} />
        {:else if active.id === 'access'}
          <Access onExpired={expire} />
        {:else}
          {#key active.id}
            <Repository section={active.id} onExpired={expire} />
          {/key}
        {/if}
      </section>
    </main>
  </div>
{/if}
