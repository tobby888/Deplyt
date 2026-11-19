<script>
  import { onMount } from 'svelte'
  import Deployments from './Deployments.svelte'
  import Repository from './Repository.svelte'
  import { api, repository } from './api.js'

  const sections = [
    { id: 'deployments', label: '部署记录', icon: '◈' },
    { id: 'commits', label: '提交', icon: '⎇' },
    { id: 'pulls', label: 'Pull Requests', icon: '⇆' },
    { id: 'actions', label: 'Actions', icon: '▷' },
    { id: 'security', label: '安全告警', icon: '♢' },
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
  let active = $derived(sections.find(({ id }) => section === id))

  onMount(async () => {
    try {
      user = await api('/api/session')
    } catch (err) {
      if (err.status !== 401) error = err.message
    } finally {
      loading = false
    }
  })

  function expire() {
    user = null
    error = '管理员会话已过期，请重新登录。'
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
      <div class="brand"><span class="mark" aria-hidden="true">D</span> Deplyt</div>
      <h1>PhotoLib 部署控制台</h1>
      {#if loading}
        <p role="status">正在检查登录状态…</p>
      {:else}
        <p>使用 PhotoLib 管理员账号登录，查看发布记录及仓库活动。</p>
        {#if error || loginFailed}
          <p class="notice danger" role="alert">{error || '授权未完成，请重新发起登录。'}</p>
        {/if}
        <a class="button primary" href="/auth/start">使用 PhotoLib 登录 <span aria-hidden="true">↗</span></a>
      {/if}
    </section>
  </main>
{:else}
  <div class="layout">
    <aside class="sidebar">
      <a class="brand" href="#deployments"><span class="mark" aria-hidden="true">D</span> Deplyt</a>
      <nav aria-label="控制台">
        {#each sections as item (item.id)}
          <a href={'#' + item.id} aria-current={section === item.id ? 'page' : undefined}>
            <span class="nav-icon" aria-hidden="true">{item.icon}</span>{item.label}
          </a>
        {/each}
      </nav>
      <a class="repository-link" href={repository} target="_blank" rel="noreferrer">GitHub 仓库 ↗</a>
    </aside>
    <main class="workspace">
      <header class="topbar">
        <span>部署控制台 <span class="muted">/</span> <strong>PhotoLib</strong></span>
        <div class="account"><span title="PhotoLib 管理员">{user.username}</span><button class="button compact" onclick={logout}>退出</button></div>
      </header>
      <section class="content" aria-labelledby="section-title">
        <div class="page-heading">
          <h1 id="section-title">{active.label}</h1>
          <p class="muted">{section === 'deployments' ? '查看生产发布、OSS 归档和版本回滚。' : 'tobby888 / Photolib'}</p>
        </div>
        {#if error}<p class="notice danger" role="alert">{error}</p>{/if}
        {#if section === 'deployments'}
          <Deployments onExpired={expire} />
        {:else}
          {#key section}
            <Repository {section} onExpired={expire} />
          {/key}
        {/if}
      </section>
    </main>
  </div>
{/if}
