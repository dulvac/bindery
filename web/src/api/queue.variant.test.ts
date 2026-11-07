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
  await queueApi.grabPending(10, { acknowledgeRatioCost: true, variantId: 17, languageOverrideReason: 'Reviewed release tag' })
  expect(bodies).toEqual([{ acknowledgeRatioCost: true, variantId: 17, languageOverrideReason: 'Reviewed release tag' }])
})

it('posts explicit ratio acknowledgement for legacy pending grabs without a variant', async () => {
  const bodies: unknown[] = []
  server.use(http.post(apiUrl('/pending/10/grab'), async ({ request }) => {
    bodies.push(await request.json())
    return HttpResponse.json({ id: 8 })
  }))
  await queueApi.grabPending(10, { acknowledgeRatioCost: true })
  expect(bodies).toEqual([{ acknowledgeRatioCost: true }])
})

it('posts acknowledgement and the chosen variant without an override when none was given', async () => {
  const bodies: unknown[] = []
  server.use(http.post(apiUrl('/pending/10/grab'), async ({ request }) => {
    bodies.push(await request.json())
    return HttpResponse.json({ id: 8 })
  }))
  await queueApi.grabPending(10, { acknowledgeRatioCost: true, variantId: 17 })
  expect(bodies).toEqual([{ acknowledgeRatioCost: true, variantId: 17 }])
})
