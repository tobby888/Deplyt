<script>
  import { onMount } from 'svelte'
  import { api, date, githubLink } from './api.js'

  let { section, onExpired } = $props()
  const securitySources = [
    { id: 'dependabot', label: 'Dependabot' },
    { id: 'code', label: 'Code scanning' },
    { id: 'secrets', label: 'Secret scanning' },
  ]
  let source = $state('dependabot')
  let feed = $derived(section === 'security' ? source : section)
  let items = $state([])
  let page = $state(1)
  let next = $state(false)
  let loading = $state(true)
  let error = $state('')
  let details = $state({})
  const states = {
    open: '开放', closed: '已关闭', merged: '已合并', dismissed: '已忽略', fixed: '已修复',
    resolved: '已处理', success: '成功', failure: '失败', cancelled: '已取消', skipped: '已跳过',
    in_progress: '执行中', queued: '排队中', completed: '已完成', timed_out: '超时',
    critical: '严重', high: '高危', moderate: '中危', medium: '中危', low: '低危',
  }
  const statusClass = {
    merged: 'succeeded', success: 'succeeded', failure: 'failed', timed_out: 'failed',
    in_progress: 'running', queued: 'running', open: 'running', critical: 'failed', high: 'failed',
  }

  async function load(to = page) {
    loading = true
    error = ''
    details = {}
    try {
      const result = await api(`/api/github/${feed}?page=${to}`)
      items = result.items
      next = result.hasNext
      page = to
    } catch (err) {
      items = []
      next = false
      if (err.status === 401) onExpired()
      else error = err.message
    } finally {
      loading = false
    }
  }

  onMount(() => { void load(1) })

  function select(id) {
    if (source === id) return
    source = id
    items = []
    page = 1
    void load(1)
  }

  async function loadDetail(event, id) {
    if (!event.currentTarget.open || details[id]) return
    const cache = details
    cache[id] = { loading: true }
    try {
      cache[id] = { data: await api(`/api/github/${section}/${id}`) }
    } catch (err) {
      if (err.status === 401) onExpired()
      else cache[id] = { error: err.message }
    }
  }

  function titleFor(alert) {
    if (feed === 'dependabot') return alert.security_advisory?.summary ?? '依赖安全告警'
    if (feed === 'code') return alert.rule?.description ?? '代码扫描告警'
    return alert.secret_type_display_name || '凭据扫描告警'
  }

  function prState(pr) {
    return pr.merged_at ? 'merged' : pr.state
  }
</script>

{#snippet badge(value)}
  {#if value}<span class={'status ' + (statusClass[value] ?? '')}>{states[value] ?? value}</span>{/if}
{/snippet}

{#snippet external(url)}
  {#if githubLink(url)}<a class="external" href={githubLink(url)} target="_blank" rel="noreferrer">GitHub ↗</a>{/if}
{/snippet}

{#snippet fact(label, value)}
  {#if value || value === 0}
    <div><dt>{label}</dt><dd>{value}</dd></div>
  {/if}
{/snippet}

{#snippet detailState(entry)}
  {#if entry?.loading}<p class="muted" role="status">加载详情…</p>
  {:else if entry?.error}<p class="notice danger" role="alert">{entry.error}</p>{/if}
{/snippet}

<section class="panel">
  <div class="panel-heading">
    {#if section === 'security'}
      <div class="switcher" role="group" aria-label="告警来源">
        {#each securitySources as item (item.id)}
          <button class="button compact" disabled={loading} aria-pressed={source === item.id} onclick={() => select(item.id)}>{item.label}</button>
        {/each}
      </div>
    {:else}
      <h2>{section === 'commits' ? '默认分支提交' : section === 'pulls' ? '全部 Pull Requests' : '工作流运行'}</h2>
    {/if}
    <button class="button" disabled={loading} onclick={() => load()}>刷新</button>
  </div>

  {#if loading}
    <p class="empty" role="status">正在读取 GitHub 数据…</p>
  {:else if error}
    <p class="empty danger" role="alert">{error}</p>
  {:else if !items.length}
    <p class="empty">暂无记录。</p>
  {:else}
    {#each items as item (item.sha ?? item.id ?? item.number)}
      <div class="row">
        <div class="row-content">
          {#if section === 'commits'}
            {@const entry = details[item.sha]}
            <h3>{item.commit.message.split('\n')[0]}</h3>
            <p class="metadata"><code>{item.sha.slice(0, 12)}</code><span>{item.author?.login ?? item.commit.author?.name}</span><span>{date(item.commit.author?.date)}</span>
              {#if item.commit.verification?.verified}<span class="status succeeded">签名已验证</span>{/if}
            </p>
            <details ontoggle={(event) => loadDetail(event, item.sha)}>
              <summary>提交详情</summary>
              <pre>{item.commit.message}</pre>
              {@render detailState(entry)}
              {#if entry?.data}
                <dl class="facts">
                  {@render fact('完整 SHA', item.sha)}
                  {@render fact('提交者', entry.data.commit?.committer?.name)}
                  {@render fact('变更', `+${entry.data.stats?.additions ?? 0} / −${entry.data.stats?.deletions ?? 0}`)}
                </dl>
                {#if entry.data.files?.length}
                  <ul class="files">
                    {#each entry.data.files as file (file.filename)}
                      <li><code>{file.filename}</code> <span class="muted">+{file.additions} / −{file.deletions}</span></li>
                    {/each}
                  </ul>
                {/if}
              {/if}
            </details>
          {:else if section === 'pulls'}
            {@const entry = details[item.number]}
            <h3><span class="muted">#{item.number}</span> {item.title}</h3>
            <p class="metadata">
              {@render badge(item.draft ? '草稿' : prState(item))}
              <span>{item.user?.login}</span><code>{item.head?.ref} → {item.base?.ref}</code><span>更新于 {date(item.updated_at)}</span>
            </p>
            <details ontoggle={(event) => loadDetail(event, item.number)}>
              <summary>PR 详情</summary>
              {@render detailState(entry)}
              <dl class="facts">
                {@render fact('创建时间', date(item.created_at))}
                {@render fact('审阅者', item.requested_reviewers?.map(user => user.login).join('、'))}
                {@render fact('标签', item.labels?.map(label => label.name).join('、'))}
                {#if entry?.data}
                  {@render fact('提交数', entry.data.commits)}
                  {@render fact('变更文件', entry.data.changed_files)}
                  {@render fact('变更', `+${entry.data.additions} / −${entry.data.deletions}`)}
                  {#if entry.data.merged_at}{@render fact('合并时间', date(entry.data.merged_at))}{/if}
                {/if}
              </dl>
              {#if item.body}<pre>{item.body}</pre>{/if}
            </details>
          {:else if section === 'actions'}
            {@const entry = details[item.id]}
            <h3>{item.display_title || item.name}</h3>
            <p class="metadata">{@render badge(item.conclusion || item.status)}<span>{item.name} #{item.run_number}</span><code>{item.head_branch}</code><span>{date(item.created_at)}</span></p>
            <details ontoggle={(event) => loadDetail(event, item.id)}>
              <summary>运行详情</summary>
              <dl class="facts">
                {@render fact('触发者', item.triggering_actor?.login || item.actor?.login)}
                {@render fact('事件', item.event)}
                {@render fact('运行尝试', item.run_attempt)}
                {@render fact('提交', item.head_sha)}
                {@render fact('更新时间', date(item.updated_at))}
              </dl>
              {@render detailState(entry)}
              {#if entry?.data?.jobs?.length}
                <ul class="files">
                  {#each entry.data.jobs as job (job.id)}
                    <li>{@render badge(job.conclusion || job.status)} {job.name} {@render external(job.html_url)}</li>
                  {/each}
                </ul>
              {/if}
            </details>
          {:else}
            <h3><span class="muted">#{item.number}</span> {titleFor(item)}</h3>
            <p class="metadata">
              {@render badge(item.state)}
              {@render badge(item.security_vulnerability?.severity || item.rule?.security_severity_level || item.rule?.severity)}
              <span>{item.dependency?.package?.name || item.tool?.name || item.validity || ''}</span>
              <span>{date(item.created_at)}</span>
            </p>
            <details>
              <summary>告警详情</summary>
              <dl class="facts">
                {@render fact('依赖清单', item.dependency?.manifest_path)}
                {@render fact('受影响版本', item.security_vulnerability?.vulnerable_version_range)}
                {@render fact('修复版本', item.security_vulnerability?.first_patched_version?.identifier)}
                {@render fact('CVE', item.security_advisory?.cve_id)}
                {@render fact('规则', item.rule?.id)}
                {@render fact('文件', item.most_recent_instance?.location?.path)}
                {@render fact('分支引用', item.most_recent_instance?.ref)}
                {@render fact('处理结果', item.resolution || item.dismissed_reason)}
                {@render fact('更新时间', date(item.updated_at))}
              </dl>
            </details>
          {/if}
        </div>
        {@render external(item.html_url)}
      </div>
    {/each}
  {/if}
  {#if page > 1 || next}
    <div class="pagination">
      <button class="button compact" disabled={loading || page === 1} onclick={() => load(page - 1)}>上一页</button>
      <span>第 {page} 页</span>
      <button class="button compact" disabled={loading || !next} onclick={() => load(page + 1)}>下一页</button>
    </div>
  {/if}
</section>
