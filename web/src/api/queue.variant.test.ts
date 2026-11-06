import { expect, it } from 'vitest'
import { http, HttpResponse } from 'msw'
import { apiUrl, server } from '../test/msw'
import { queueApi } from './queue'

it('posts a real pending variant and an optional audited language override reason', async () => {
  const bodies: unknown[] = []
  server.use(http.post(apiUrl('/pending/10/grab'), async ({ request }) => {
    bodies.push(await request.json())
    return HttpResponse.json({ id: 8 })
  }))
  await queueApi.grabPending(10, { variantId: 17, languageOverrideReason: 'Reviewed release tag' })
  expect(bodies).toEqual([{ variantId: 17, languageOverrideReason: 'Reviewed release tag' }])
})

it('leaves the legacy pending grab body absent', async () => {
  let body = 'not called'
  server.use(http.post(apiUrl('/pending/10/grab'), async ({ request }) => {
    body = await request.text()
    return HttpResponse.json({ id: 8 })
  }))
  await queueApi.grabPending(10)
  expect(body).toBe('')
})
