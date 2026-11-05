import { beforeEach, describe, expect, it } from 'vitest'
import { http, HttpResponse } from 'msw'
import { apiUrl, server } from '../test/msw'
import { requestsApi } from './requests'

interface Seen {
  method: string
  path: string
  body: unknown
}

function record(): Seen[] {
  const seen: Seen[] = []
  server.use(
    http.all(apiUrl('*'), async ({ request }) => {
      const url = new URL(request.url)
      const text = request.method === 'GET' ? '' : await request.text()
      seen.push({
        method: request.method,
        path: url.pathname.replace('/api/v1', ''),
        body: text ? JSON.parse(text) : undefined,
      })
      return HttpResponse.json({})
    }),
  )
  return seen
}

describe('requestsApi request shapes', () => {
  beforeEach(() => {
    window.history.pushState(null, '', '/')
  })

  it('defaults a book request to English and preserves explicit language choices', async () => {
    const seen = record()

    await requestsApi.createRequest('book', 'OL1W', 'both')
    await requestsApi.createRequest('book', 'OL2W', 'ebook', ['eng', 'rum'])

    expect(seen.map(s => [s.method, s.path, s.body])).toEqual([
      ['POST', '/requests', { kind: 'book', foreignId: 'OL1W', mediaType: 'both', languages: ['eng'] }],
      ['POST', '/requests', { kind: 'book', foreignId: 'OL2W', mediaType: 'ebook', languages: ['eng', 'rum'] }],
    ])
  })

  it('omits languages on author requests', async () => {
    const seen = record()

    await requestsApi.createRequest('author', 'OL39307A')

    expect(seen).toHaveLength(1)
    expect(seen[0]).toMatchObject({
      method: 'POST',
      path: '/requests',
      body: { kind: 'author', foreignId: 'OL39307A', mediaType: '' },
    })
    expect((seen[0].body as Record<string, unknown>)).not.toHaveProperty('languages')
  })
})
