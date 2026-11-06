import { useCallback, useEffect, useState } from 'react'
import { Link } from 'react-router'
import { useTranslation } from 'react-i18next'
import { api } from '../../api/client'
import type { RequesterLibraryBook } from '../../api/client'
import { mediaTypeLabel } from './requestLabels'
import CoverImage from '../../components/CoverImage'
import BookLanguages from '../../components/BookLanguages'
import BookVariantStatus from '../../components/BookVariantStatus'
import { primaryFill } from '../../components/buttons'

const PAGE_SIZE = 60

function hasMissingSelection(book: RequesterLibraryBook | undefined, mediaType: string, languages: string[]): boolean {
  if (!book || languages.length === 0) return false
  const formats = mediaType === 'both' ? ['ebook', 'audiobook'] : [mediaType]
  const existing = new Set((book.variants ?? []).map(v => `${v.mediaType}/${v.language.trim().toLowerCase()}`))
  return formats.some(format => languages.some(language => !existing.has(`${format}/${language}`)))
}

// The library as a requester sees it: browsed through the scoped projection,
// with no links into book pages and no downloads. Owned books can
// start a missing-language request directly without a My Requests row.
export default function RequesterLibraryPage() {
  const { t } = useTranslation()
  const [query, setQuery] = useState('')
  const [search, setSearch] = useState('')
  const [items, setItems] = useState<RequesterLibraryBook[]>([])
  const [total, setTotal] = useState(0)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [editingBookId, setEditingBookId] = useState<number | null>(null)
  const [editMediaType, setEditMediaType] = useState('ebook')
  const [editLanguages, setEditLanguages] = useState<string[]>(['eng'])
  const [sending, setSending] = useState(false)
  const [sentBookId, setSentBookId] = useState<number | null>(null)

  const load = useCallback(async (offset: number, term: string) => {
    setLoading(true)
    setError(null)
    try {
      const page = await api.requesterLibrary({ search: term, limit: PAGE_SIZE, offset })
      setItems(prev => (offset === 0 ? page.items : [...prev, ...page.items]))
      setTotal(page.total)
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : t('requests.library.loadFailed'))
    } finally {
      setLoading(false)
    }
  }, [t])

  useEffect(() => { void load(0, search) }, [load, search])

  const openAdd = (book: RequesterLibraryBook) => {
    if (editingBookId === book.id) {
      setEditingBookId(null)
      return
    }
    const format = book.formats[0] === 'audiobook' ? 'audiobook' : 'ebook'
    const existing = (book.variants ?? []).filter(v => v.mediaType === format).map(v => v.language)
    setEditMediaType(format)
    setEditLanguages(existing.length > 0 ? Array.from(new Set(existing)) : ['eng'])
    setEditingBookId(book.id)
    setSentBookId(null)
  }

  const sendAdd = async (book: RequesterLibraryBook) => {
    if (!book.foreignBookId || !hasMissingSelection(book, editMediaType, editLanguages)) return
    setSending(true)
    setError(null)
    try {
      await api.createRequest('book', book.foreignBookId, editMediaType, editLanguages)
      setEditingBookId(null)
      setSentBookId(book.id)
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : t('requests.mine.extendFailed'))
    } finally {
      setSending(false)
    }
  }

  return (
    <div className="space-y-6">
      <div className="flex items-center justify-between gap-4 flex-wrap">
        <h2 className="text-2xl font-bold">{t('requests.library.title')}</h2>
        <Link to="/request" className={`px-4 py-2 ${primaryFill} rounded-md text-sm font-medium`}>
          {t('requests.library.requestSomething')}
        </Link>
      </div>

      <form
        role="search"
        onSubmit={e => { e.preventDefault(); setSearch(query.trim()) }}
        className="flex gap-2 max-w-md"
      >
        <input
          enterKeyHint="search"
          type="search"
          value={query}
          onChange={e => setQuery(e.target.value)}
          aria-label={t('requests.library.searchLabel')}
          placeholder={t('requests.library.searchPlaceholder')}
          className="flex-1 min-w-0 bg-slate-200 dark:bg-zinc-800 border border-slate-300 dark:border-zinc-700 rounded-md px-3 py-2 text-sm"
        />
        <button type="submit" className="px-4 py-2 rounded-md text-sm border border-slate-300 dark:border-zinc-700">{t('common.search')}</button>
      </form>

      {error && <p role="alert" className="text-sm text-red-700 dark:text-red-300">{error}</p>}
      {!loading && !error && items.length === 0 && (
        <p className="text-sm text-fg-muted">{search ? t('requests.library.noMatches') : t('requests.library.empty')}</p>
      )}

      <ul className="grid grid-cols-2 sm:grid-cols-3 md:grid-cols-4 lg:grid-cols-6 gap-4" aria-label={t('requests.library.title')}>
        {items.map(b => (
          <li key={b.id} className="min-w-0">
            <CoverImage src={b.coverUrl} alt="" loading="lazy" decoding="async" className="w-full aspect-[2/3] object-cover rounded-md bg-slate-200 dark:bg-zinc-800"
              fallback={
                <div aria-hidden="true" className="w-full aspect-[2/3] rounded-md bg-slate-200 dark:bg-zinc-800" />
              }
            />
            <div className="mt-2 text-sm font-medium leading-snug [overflow-wrap:anywhere]">{b.title}</div>
            <div className="text-xs text-fg-muted [overflow-wrap:anywhere]">{b.authorName}</div>
            {b.series && (
              <div className="text-xs text-fg-muted [overflow-wrap:anywhere]">
                {b.seriesPosition ? t('requests.library.seriesWithPosition', { series: b.series, position: b.seriesPosition }) : b.series}
              </div>
            )}
            <div className="text-xs mt-1">
              {b.formats.length > 0
                ? b.formats.map(f => mediaTypeLabel(t, f)).join(', ')
                : t('requests.library.notYetAvailable')}
            </div>
            <BookVariantStatus variants={b.variants} legacyFiles={b.legacyFiles} className="mt-1" />
            {b.foreignBookId && (
              <>
                <button
                  type="button"
                  className="mt-2 text-xs text-accent-text hover:underline"
                  onClick={() => openAdd(b)}
                >
                  {t('requests.mine.addLanguage')}
                </button>
                {editingBookId === b.id && (
                  <div className="mt-2 p-2 rounded-md border border-slate-300 dark:border-zinc-700 space-y-3">
                    <label className="flex flex-col gap-1 text-xs">
                      <span>{t('requests.confirm.format')}</span>
                      <select
                        aria-label={t('requests.confirm.formatLabel')}
                        value={editMediaType}
                        onChange={event => setEditMediaType(event.target.value)}
                        disabled={sending}
                        className="bg-slate-200 dark:bg-zinc-800 border border-slate-300 dark:border-zinc-700 rounded px-2 py-1"
                      >
                        <option value="ebook">{t('common.ebook')}</option>
                        <option value="audiobook">{t('common.audiobook')}</option>
                        <option value="both">{t('common.both')}</option>
                      </select>
                    </label>
                    <BookLanguages value={editLanguages} onChange={setEditLanguages} disabled={sending} />
                    <button
                      type="button"
                      onClick={() => void sendAdd(b)}
                      disabled={sending || !hasMissingSelection(b, editMediaType, editLanguages)}
                      className={`px-3 py-1.5 ${primaryFill} disabled:opacity-50 disabled:cursor-not-allowed rounded-md text-xs font-medium`}
                    >
                      {sending ? t('requests.confirm.sending') : t('requests.mine.requestLanguage')}
                    </button>
                  </div>
                )}
                {sentBookId === b.id && <p role="status" className="mt-1 text-xs text-fg-muted">{t('requests.library.languageRequested')}</p>}
              </>
            )}
          </li>
        ))}
      </ul>

      {loading && <p className="text-sm text-fg-muted">{t('common.loading')}</p>}
      {!loading && items.length < total && (
        <button type="button" onClick={() => void load(items.length, search)} className="px-4 py-2 text-sm rounded-md border border-slate-300 dark:border-zinc-700">
          {t('requests.loadMore')}
        </button>
      )}
    </div>
  )
}
