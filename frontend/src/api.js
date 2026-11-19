export const repository = 'https://github.com/tobby888/Photolib'
export const formatDate = new Intl.DateTimeFormat('zh-CN', {
  timeZone: 'Asia/Shanghai', dateStyle: 'medium', timeStyle: 'short',
})

export function date(value) {
  return value ? formatDate.format(new Date(value)) : '—'
}

export function githubLink(value) {
  if (typeof value !== 'string') return undefined
  try {
    const url = new URL(value)
    if (url.origin === 'https://github.com') return url.href
  } catch {
    return undefined
  }
}

export async function api(path, options) {
  const response = await fetch(path, options)
  if (!response.ok) {
    const error = new Error((await response.text()).trim() || `请求失败（HTTP ${response.status}）`)
    error.status = response.status
    throw error
  }
  return response.status === 204 ? null : response.json()
}
