import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, fireEvent, waitFor } from '@testing-library/react'
import AddBookConfirm from './AddBookConfirm'
import AddAuthorConfirm from './AddAuthorConfirm'

// For a requester the Add Book and Add Author confirm steps become a request:
// only the request payload is sent, nothing is added, and none of the admin
// lookups run.

vi.mock('react-i18next', () => ({
  useTranslation: () => ({
    t: (key: string, fallback?: unknown) => {
      const strings: Record<string, string> = {
        'requests.confirm.requestBook': 'Request book',
        'requests.confirm.requestAuthor': 'Request author',
        'requests.confirm.formatLabel': 'Format to request',
        'requests.confirm.bookHint': 'An admin will look at your request.',
        'requests.confirm.authorHint': 'An admin will look at your request.',
        'common.ebook': 'Ebook',
        'common.audiobook': 'Audiobook',
        'common.both': 'Both',
        'common.cancel': 'Cancel',
        'bookLanguages.label': 'Languages',
        'bookLanguages.english': 'English',
        'bookLanguages.french': 'French',
        'bookLanguages.romanian': 'Romanian',
      }
      return strings[key] ?? (typeof fallback === 'string' ? fallback : key)
    },
  }),
}))

vi.mock('../auth/AuthContext', () => ({
  useIsRequester: () => true,
}))

vi.mock('../api/client', () => ({
  api: {
    createRequest: vi.fn(),
    addBook: vi.fn(),
    addAuthor: vi.fn(),
    listIndexers: vi.fn(),
    listDownloadClients: vi.fn(),
    listMetadataProfiles: vi.fn(),
    listRootFolders: vi.fn(),
    getSetting: vi.fn(),
  },
}))

import { api } from '../api/client'
import type { Author, Book, LibraryRequest } from '../api/client'

const created: LibraryRequest = {
  id: 7,
  kind: 'book',
  foreignId: 'OL1W',
  mediaType: 'audiobook',
  title: 'Dune',
  authorName: 'Frank Herbert',
  status: 'pending',
  createdAt: '2026-09-17T00:00:00Z',
  fulfilled: false,
  languages: ['eng', 'rum'],
}

const dune = {
  id: 0, foreignBookId: 'OL1W', authorId: 0, title: 'Dune', description: '', imageUrl: '', genres: [],
  monitored: true, status: 'wanted', filePath: '', mediaType: 'ebook', ebookFilePath: '', audiobookFilePath: '',
  excluded: false, author: { authorName: 'Frank Herbert', foreignAuthorId: 'OL-FH' },
} as unknown as Book

const herbert = { foreignAuthorId: 'OL-FH', authorName: 'Frank Herbert', disambiguation: 'Dune' } as unknown as Author

describe('confirm steps for a requester', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    vi.mocked(api.createRequest).mockResolvedValue(created)
  })

  it('sends one language array for a book request, even when both formats are requested', async () => {
    const onRequested = vi.fn()
    const onAdded = vi.fn()
    render(<AddBookConfirm book={dune} searchedISBN={null} onBack={vi.fn()} onClose={vi.fn()} onAdded={onAdded} onRequested={onRequested} />)

    const english = screen.getByRole('checkbox', { name: 'English' })
    const romanian = screen.getByRole('checkbox', { name: 'Romanian' })
    expect(english).toBeChecked()
    expect(romanian).not.toBeChecked()

    fireEvent.click(romanian)
    fireEvent.change(screen.getByRole('combobox', { name: 'Format to request' }), { target: { value: 'both' } })
    fireEvent.click(screen.getByRole('button', { name: 'Request book' }))

    await waitFor(() => expect(onRequested).toHaveBeenCalledWith(created))
    expect(api.createRequest).toHaveBeenCalledWith('book', 'OL1W', 'both', ['eng', 'rum'])
    expect(api.addBook).not.toHaveBeenCalled()
    expect(onAdded).not.toHaveBeenCalled()
  })

  it('disables the request button when no language is selected', () => {
    render(<AddBookConfirm book={dune} searchedISBN={null} onBack={vi.fn()} onClose={vi.fn()} onAdded={vi.fn()} />)

    fireEvent.click(screen.getByRole('checkbox', { name: 'English' }))
    const request = screen.getByRole('button', { name: 'Request book' })
    expect(request).toBeDisabled()
    fireEvent.click(request)
    expect(api.createRequest).not.toHaveBeenCalled()
  })

  it('sends an author request with no format or language selector', async () => {
    const onRequested = vi.fn()
    render(<AddAuthorConfirm author={herbert} defaults={null} primaryProvider={null} onBack={vi.fn()} onClose={vi.fn()} onAdded={vi.fn()} onRequested={onRequested} />)

    expect(screen.queryByRole('combobox', { name: 'Format to request' })).not.toBeInTheDocument()
    expect(screen.queryByRole('checkbox', { name: 'English' })).not.toBeInTheDocument()

    fireEvent.click(screen.getByRole('button', { name: 'Request author' }))

    await waitFor(() => expect(onRequested).toHaveBeenCalled())
    const call = vi.mocked(api.createRequest).mock.calls[0]
    expect(call).toEqual(['author', 'OL-FH', ''])
    expect(api.addAuthor).not.toHaveBeenCalled()
    expect(api.listIndexers).not.toHaveBeenCalled()
    expect(api.listDownloadClients).not.toHaveBeenCalled()
  })

  it('shows the server sentence when the request is refused', async () => {
    vi.mocked(api.createRequest).mockRejectedValue(new Error('This book is already in the library.'))
    render(<AddBookConfirm book={dune} searchedISBN={null} onBack={vi.fn()} onClose={vi.fn()} onAdded={vi.fn()} />)
    fireEvent.click(screen.getByRole('button', { name: 'Request book' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('This book is already in the library.')
  })
})
