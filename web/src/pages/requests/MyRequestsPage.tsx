import { useCallback, useEffect, useMemo, useState } from 'react'
import { Link } from 'react-router'
import { useTranslation } from 'react-i18next'
import { api } from '../../api/client'
import type { LibraryRequest } from '../../api/client'
import { mediaTypeLabel, requestStatusClass, requestStatusLabel } from './requestLabels'
import { primaryFill } from '../../components/buttons'
import BookLanguages from '../../components/BookLanguages'
import BookVariantStatus from '../../components/BookVariantStatus'

const PAGE_SIZE = 50

function requestFormatsFromMediaType(mediaType: string): Array<'ebook' | 'audiobook'> {
  if (mediaType === 'both') return ['ebook', 'audiobook']
  if (mediaType === 'audiobook') return ['audiobook']
  if (mediaType === 'ebook') return ['ebook']
  return []
}

function requestFormatsFromRequest(request: LibraryRequest, overrideMediaType: string): Array<'ebook' | 'audiobook'> {
  if (overrideMediaType) return requestFormatsFromMediaType(overrideMediaType)
  if (request.mediaType) return requestFormatsFromMediaType(request.mediaType)
  const fromVariants = Array.from(
    new Set(
      (request.variants ?? [])
        .map(v => v.mediaType)
        .filter((v): v is 'ebook' | 'audiobook' => v === 'ebook' || v === 'audiobook'),
    ),
  )
  return fromVariants
}

function pendingVariants(request: LibraryRequest): NonNullable<LibraryRequest['variants']> {
  if (!request.mediaType) return request.variants ?? []
  const existing = new Map((request.variants ?? []).map(v => [`${v.mediaType}/${v.language}`, v]))
  const rows: NonNullable<LibraryRequest['variants']> = []
  for (const format of requestFormatsFromMediaType(request.mediaType)) {
    for (const language of request.languages ?? []) {
      rows.push(existing.get(`${format}/${language}`) ?? {
        id: -(rows.length + 1), mediaType: format, language, status: 'wanted', verification: '',
      })
    }
  }
  return rows
}

function hasMissingSelection(request: LibraryRequest, mediaType: string, languages: string[]): boolean {
  if (languages.length === 0) return false
  const formats = requestFormatsFromRequest(request, mediaType)
  const existing = new Set(
    (request.variants ?? []).map(v => `${v.mediaType}/${(v.language || '').trim().toLowerCase()}`),
  )
  if (request.status === 'pending') {
    for (const format of requestFormatsFromMediaType(request.mediaType)) {
      for (const language of request.languages ?? []) existing.add(`${format}/${language}`)
    }
  }
  return formats.some(format => languages.some(language => !existing.has(`${format}/${language}`)))
}

// A requester's own requests, newest first, with what became of each one.
// A pending request can be withdrawn.
export default function MyRequestsPage() {
  const { t } = useTranslation()
  const [items, setItems] = useState<LibraryRequest[]>([])
  const [total, setTotal] = useState(0)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [editingRequestId, setEditingRequestId] = useState<number | null>(null)
  const [editMediaType, setEditMediaType] = useState('')
  const [editLanguages, setEditLanguages] = useState<string[]>(['eng'])
  const [sendingExtend, setSendingExtend] = useState(false)

  const load = useCallback(async (offset: number) => {
    setLoading(true)
    setError(null)
    try {
      const page = await api.listMyRequests({ limit: PAGE_SIZE, offset })
      setItems(prev => (offset === 0 ? page.items : [...prev, ...page.items]))
      setTotal(page.total)
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : t('requests.mine.loadFailed'))
    } finally {
      setLoading(false)
    }
  }, [t])

  useEffect(() => { void load(0) }, [load])

  const withdraw = async (r: LibraryRequest) => {
    try {
      await api.withdrawRequest(r.id)
      setItems(prev => prev.filter(x => x.id !== r.id))
      setTotal(n => Math.max(0, n - 1))
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : t('requests.mine.withdrawFailed'))
    }
  }

  const openExtend = (request: LibraryRequest) => {
    if (editingRequestId === request.id) {
      setEditingRequestId(null)
      return
    }
    setEditingRequestId(request.id)
    setEditMediaType(request.mediaType || 'ebook')
    setEditLanguages(request.languages && request.languages.length > 0 ? request.languages : ['eng'])
  }

  const editingRequest = useMemo(
    () => items.find(item => item.id === editingRequestId) ?? null,
    [items, editingRequestId],
  )

  const canSubmitExtend = editingRequest
    ? hasMissingSelection(editingRequest, editMediaType, editLanguages)
    : false

  const sendExtend = async (request: LibraryRequest) => {
    if (request.kind !== 'book' || editLanguages.length === 0 || !hasMissingSelection(request, editMediaType, editLanguages)) return
    setSendingExtend(true)
    setError(null)
    try {
      const created = await api.createRequest('book', request.foreignId, editMediaType, editLanguages)
      const touchesRequest = (item: LibraryRequest) => item.id === request.id || (item.kind === 'book' && item.foreignId === request.foreignId)

      if (created.status === 'approved' && created.variants === undefined) {
        try {
          const visibleCount = Math.max(PAGE_SIZE, items.length)
          const refreshed: LibraryRequest[] = []
          let refreshedTotal = 0
          while (refreshed.length < visibleCount) {
            const limit = Math.min(PAGE_SIZE, visibleCount - refreshed.length)
            const page = await api.listMyRequests({ limit, offset: refreshed.length })
            refreshed.push(...page.items)
            refreshedTotal = page.total
            if (page.items.length === 0 || refreshed.length >= refreshedTotal) break
          }
          setItems(refreshed)
          setTotal(refreshedTotal)
        } catch {
          setItems(prev => prev.map(item => (touchesRequest(item) ? created : item)))
          setError(t('requests.mine.statusUnknownReload'))
        }
        setEditingRequestId(null)
        return
      }

      setItems(prev => prev.map(item =>
        touchesRequest(item)
          ? { ...created, variants: created.variants ?? item.variants, legacyFiles: created.legacyFiles ?? item.legacyFiles }
          : item,
      ))
      setEditingRequestId(null)
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : t('requests.mine.extendFailed', 'Could not send the language request'))
    } finally {
      setSendingExtend(false)
    }
  }

  return (
    <div className="space-y-6">
      <div className="flex items-center justify-between gap-4 flex-wrap">
        <h2 className="text-2xl font-bold">{t('requests.mine.title')}</h2>
        <Link to="/request" className={`px-4 py-2 ${primaryFill} rounded-md text-sm font-medium`}>
          {t('requests.mine.newRequest')}
        </Link>
      </div>

      {error && <p role="alert" className="text-sm text-red-700 dark:text-red-300">{error}</p>}

      {!loading && items.length === 0 && !error && (
        <p className="text-sm text-fg-muted">{t('requests.mine.empty')}</p>
      )}

      {items.length > 0 && (
        <ul className="divide-y divide-slate-200 dark:divide-zinc-800 border border-slate-200 dark:border-zinc-800 rounded-lg" aria-label={t('requests.mine.title')}>
          {items.map(r => (
            <li key={r.id} className="p-4 flex items-start justify-between gap-4">
              <div className="min-w-0">
                <div className="font-medium [overflow-wrap:anywhere]">{r.title}</div>
                <div className="text-xs text-fg-muted mt-0.5">
                  {r.kind === 'author' ? t('requests.kindAuthor') : r.authorName || t('requests.kindBook')}
                  {' · '}
                  {r.kind === 'book' && r.status === 'pending' && !r.mediaType
                    ? t('requests.variant.formatUnknown', 'Requested format unknown')
                    : mediaTypeLabel(t, r.mediaType)}
                  {' · '}
                  {new Date(r.createdAt).toLocaleDateString()}
                </div>
                {r.status === 'declined' && r.declineReason && (
                  <p className="text-xs mt-1 text-slate-700 dark:text-zinc-300">{t('requests.mine.reason', { reason: r.declineReason })}</p>
                )}
                {r.kind === 'book' && <BookVariantStatus variants={r.status === 'pending' ? pendingVariants(r) : r.variants} legacyFiles={r.legacyFiles} className="mt-2" />}
                {r.kind === 'book' && r.status !== 'declined' && (
                  <button
                    type="button"
                    className="mt-2 text-xs text-accent-text hover:underline"
                    onClick={() => openExtend(r)}
                  >
                    {t('requests.mine.addLanguage', 'Add another language')}
                  </button>
                )}

                {r.kind === 'book' && editingRequestId === r.id && (
                  <div className="mt-3 p-3 rounded-md border border-slate-300 dark:border-zinc-700 space-y-3">
                    <p className="text-xs text-fg-muted">{t('requests.mine.addLanguageHint', 'Request another language or format for this same book.')}</p>
                    <label className="flex items-center gap-2 text-sm select-none">
                      <span className="font-medium">{t('requests.confirm.format')}</span>
                      <select
                        aria-label={t('requests.confirm.formatLabel')}
                        value={editMediaType}
                        onChange={event => setEditMediaType(event.target.value)}
                        className="text-xs bg-slate-200 dark:bg-zinc-800 border border-slate-300 dark:border-zinc-700 rounded px-2 py-1"
                        disabled={sendingExtend}
                      >
                        <option value="">{t('requests.confirm.anyFormat')}</option>
                        <option value="ebook">{t('common.ebook')}</option>
                        <option value="audiobook">{t('common.audiobook')}</option>
                        <option value="both">{t('common.both')}</option>
                      </select>
                    </label>

                    <BookLanguages value={editLanguages} onChange={setEditLanguages} disabled={sendingExtend} />

                    {!canSubmitExtend && editLanguages.length > 0 && (
                      <p className="text-xs text-fg-muted">{t('requests.mine.duplicateSelection', 'That format and language selection is already requested.')}</p>
                    )}

                    <button
                      type="button"
                      onClick={() => void sendExtend(r)}
                      disabled={loading || sendingExtend || editLanguages.length === 0 || !canSubmitExtend}
                      className={`px-3 py-1.5 ${primaryFill} disabled:opacity-50 disabled:cursor-not-allowed rounded-md text-sm font-medium`}
                    >
                      {sendingExtend
                        ? t('requests.confirm.sending')
                        : t('requests.mine.requestLanguage', 'Send language request')}
                    </button>
                  </div>
                )}
              </div>
              <div className="flex items-center gap-2 flex-shrink-0">
                <span className={`px-2 py-0.5 rounded-full text-xs font-medium ${requestStatusClass(r)}`}>{requestStatusLabel(t, r)}</span>
                {r.status === 'pending' && (
                  <button
                    type="button"
                    onClick={() => void withdraw(r)}
                    aria-label={t('requests.mine.withdrawLabel', { title: r.title })}
                    className="text-xs text-fg-muted hover:text-slate-900 dark:hover:text-white"
                  >
                    {t('requests.mine.withdraw')}
                  </button>
                )}
              </div>
            </li>
          ))}
        </ul>
      )}

      {loading && <p className="text-sm text-fg-muted">{t('common.loading')}</p>}
      {!loading && items.length < total && (
        <button type="button" onClick={() => void load(items.length)} disabled={sendingExtend} className="px-4 py-2 text-sm rounded-md border border-slate-300 dark:border-zinc-700 disabled:opacity-50 disabled:cursor-not-allowed">
          {t('requests.loadMore')}
        </button>
      )}
    </div>
  )
}
