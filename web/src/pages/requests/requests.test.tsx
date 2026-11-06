import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, fireEvent, waitFor, within } from '@testing-library/react'
import { MemoryRouter } from 'react-router'
import MyRequestsPage from './MyRequestsPage'
import RequesterLibraryPage from './RequesterLibraryPage'
import RequestsPage from './RequestsPage'

vi.mock('react-i18next', () => {
  const strings: Record<string, string> = {
    'requests.mine.withdrawLabel': 'Withdraw request for {{title}}',
    'requests.mine.title': 'My requests',
    'requests.mine.newRequest': 'Request a book',
    'requests.mine.reason': 'Reason: {{reason}}',
    'requests.mine.addLanguage': 'Add another language',
    'requests.mine.addLanguageHint': 'Request another language or format for this same book.',
    'requests.mine.requestLanguage': 'Send language request',
    'requests.mine.extendFailed': 'Could not send the language request',
    'requests.mine.duplicateSelection': 'That format and language selection is already requested.',
    'requests.status.pending': 'Waiting for an admin',
    'requests.status.declined': 'Declined',
    'requests.status.available': 'Available',
    'requests.status.approved': 'Approved, not here yet',
    'requests.status.authorProgress': '{{imported}} of {{total}} books here',
    'requests.library.title': 'Library',
    'requests.library.requestSomething': 'Request a book',
    'requests.library.searchLabel': 'Search the library',
    'requests.library.searchPlaceholder': 'Title or author',
    'requests.library.empty': 'The library is empty so far.',
    'requests.library.noMatches': 'Nothing in the library matches. Try requesting it.',
    'requests.library.loadFailed': 'Could not load the library',
    'requests.library.notYetAvailable': 'Not available yet',
    'requests.library.seriesWithPosition': '{{series}} #{{position}}',
    'requests.confirm.format': 'Format',
    'requests.confirm.formatLabel': 'Format to request',
    'requests.confirm.anyFormat': 'Any format',
    'requests.confirm.sending': 'Sending…',
    'bookLanguages.english': 'English',
    'bookLanguages.french': 'French',
    'bookLanguages.romanian': 'Romanian',
    'bookLanguages.label': 'Languages',
    'common.search': 'Search',
    'common.loading': 'Loading…',
    'common.cancel': 'Cancel',
    'common.ebook': 'Ebook',
    'common.audiobook': 'Audiobook',
    'common.both': 'Both',
    'mediaType.ebook': 'Ebook',
    'mediaType.audiobook': 'Audiobook',
    'mediaType.both': 'Both',
    'bookStatus.wanted': 'Wanted',
    'bookStatus.imported': 'In library',
    'queue.status.downloading': 'Downloading',
    'bookDetail.languageUnknown': 'Language unknown',
    'requests.variant.review': 'Review',
    'requests.variant.verified': 'Verified',
    'requests.variant.unverified': 'Unverified',
    'requests.admin.title': 'Requests',
    'requests.admin.approve': 'Approve',
    'requests.admin.decline': 'Decline',
    'requests.admin.filter.pending': 'Pending',
    'requests.admin.filter.approved': 'Approved',
    'requests.admin.filter.declined': 'Declined',
    'requests.admin.filter.all': 'All',
    'requests.admin.empty': 'No requests here.',
    'requests.admin.emptyHint': 'Try another filter to see requests that were already decided.',
    'requests.admin.emptyPending': 'No requests are waiting.',
    'requests.admin.emptyPendingHint': 'New requests from your users show up here for you to approve or decline.',
    'requests.admin.approveLabel': 'Approve request for {{title}}',
    'requests.admin.declineLabel': 'Decline request for {{title}}',
    'requests.admin.approveFormLabel': 'Approve {{title}}',
    'requests.admin.declineFormLabel': 'Decline {{title}}',
    'requests.admin.confirmApprove': 'Approve and add',
    'requests.admin.confirmDecline': 'Decline',
    'requests.admin.reason': 'Reason',
    'requests.admin.format': 'Format',
    'requests.admin.searchOnAdd': 'Search indexers after adding',
    'requests.admin.metadataProfile': 'Metadata profile',
    'requests.admin.rootFolder': 'Root folder',
    'requests.admin.monitorMode': 'Monitor',
    'requests.admin.requestedBy': 'asked by {{username}} on {{date}}',
    'requests.admin.createdByAPIKey': 'made for them with the API key',
    'requests.admin.createdByAdmin': 'made for them by {{username}}',
  }
  const translation = {
    t: (key: string, opts?: unknown) => {
      const vars = (opts && typeof opts === 'object' ? opts : {}) as Record<string, unknown>
      const s = strings[key] ?? key
      return s.replace(/\{\{(\w+)\}\}/g, (_m, name: string) => String(vars[name] ?? ''))
    },
  }
  return {
    useTranslation: () => translation,
  }
})

vi.mock('../../api/client', () => ({
  api: {
    listMyRequests: vi.fn(),
    withdrawRequest: vi.fn(),
    createRequest: vi.fn(),
    requesterLibrary: vi.fn(),
    listRequestQueue: vi.fn(),
    approveRequest: vi.fn(),
    declineRequest: vi.fn(),
    listMetadataProfiles: vi.fn().mockResolvedValue([{ id: 1, name: 'Standard' }]),
    listRootFolders: vi.fn().mockResolvedValue([{ id: 4, path: '/books' }]),
    getSetting: vi.fn().mockRejectedValue(new Error('unset')),
  },
}))

import { api } from '../../api/client'
import type { LibraryRequest } from '../../api/client'

function req(overrides: Partial<LibraryRequest>): LibraryRequest {
  return {
    id: 1, kind: 'book', foreignId: 'OL1W', mediaType: '', title: 'Dune', authorName: 'Frank Herbert',
    status: 'pending', createdAt: '2026-09-17T00:00:00Z', fulfilled: false, ...overrides,
  }
}

describe('MyRequestsPage', () => {
  beforeEach(() => vi.clearAllMocks())

  it('lists what became of each request and withdraws a pending one', async () => {
    vi.mocked(api.listMyRequests).mockResolvedValue({
      items: [
        req({ id: 1, title: 'Dune' }),
        req({ id: 2, title: 'Emma', status: 'declined', declineReason: 'Not this year' }),
        req({ id: 3, title: 'Ubik', status: 'approved', fulfilled: true }),
        req({ id: 4, kind: 'author', title: 'Ursula K. Le Guin', status: 'approved', booksTotal: 5, booksImported: 2, fulfilled: true }),
      ],
      total: 4, limit: 50, offset: 0,
    })
    vi.mocked(api.withdrawRequest).mockResolvedValue(undefined)
    render(<MemoryRouter><MyRequestsPage /></MemoryRouter>)

    const list = await screen.findByRole('list', { name: 'My requests' })
    expect(within(list).getByText('Waiting for an admin')).toBeInTheDocument()
    expect(within(list).getByText('Declined')).toBeInTheDocument()
    expect(within(list).getByText('Reason: Not this year')).toBeInTheDocument()
    expect(within(list).getByText('Available')).toBeInTheDocument()
    expect(within(list).getByText('2 of 5 books here')).toBeInTheDocument()
    // Only the pending request can be withdrawn.
    expect(screen.queryByRole('button', { name: 'Withdraw request for Emma' })).not.toBeInTheDocument()

    fireEvent.click(screen.getByRole('button', { name: 'Withdraw request for Dune' }))
    await waitFor(() => expect(screen.queryByText('Dune')).not.toBeInTheDocument())
    expect(api.withdrawRequest).toHaveBeenCalledWith(1)
  })

  it('adds another language request for an existing book without dropping English', async () => {
    vi.mocked(api.listMyRequests).mockResolvedValue({
      items: [
        req({
          id: 3,
          title: 'Dune',
          mediaType: 'ebook',
          status: 'approved',
          fulfilled: true,
          languages: ['eng'],
          variants: [
            { id: 31, mediaType: 'ebook', language: 'eng', status: 'imported', verification: 'verified' },
          ],
        }),
      ],
      total: 1,
      limit: 50,
      offset: 0,
    })
    vi.mocked(api.createRequest).mockResolvedValue(
      req({
        id: 3,
        title: 'Dune',
        mediaType: 'ebook',
        status: 'pending',
        fulfilled: false,
        languages: ['eng', 'rum'],
        variants: [
          { id: 31, mediaType: 'ebook', language: 'eng', status: 'imported', verification: 'verified' },
          { id: 32, mediaType: 'ebook', language: 'rum', status: 'wanted', verification: '' },
        ],
      }),
    )

    render(<MemoryRouter><MyRequestsPage /></MemoryRouter>)

    fireEvent.click(await screen.findByRole('button', { name: 'Add another language' }))

    const submit = screen.getByRole('button', { name: 'Send language request' })
    expect(submit).toBeDisabled()

    fireEvent.click(screen.getByRole('checkbox', { name: 'Romanian' }))
    expect(submit).toBeEnabled()

    fireEvent.click(submit)
    await waitFor(() => expect(api.createRequest).toHaveBeenCalledWith('book', 'OL1W', 'ebook', ['eng', 'rum']))
    expect(await screen.findByText('Waiting for an admin')).toBeInTheDocument()

    fireEvent.click(screen.getByRole('button', { name: 'Add another language' }))
    fireEvent.click(screen.getByRole('checkbox', { name: 'English' }))
    expect(screen.getByRole('button', { name: 'Send language request' })).toBeDisabled()
  })
})

describe('RequesterLibraryPage', () => {
  beforeEach(() => vi.clearAllMocks())

  it('shows variant status labels without exposing any file paths', async () => {
    vi.mocked(api.requesterLibrary).mockResolvedValue({
      items: [
        {
          id: 1,
          title: 'Dune',
          authorName: 'Frank Herbert',
          status: 'imported',
          formats: ['ebook'],
          variants: [
            { id: 1, mediaType: 'ebook', language: 'eng', status: 'imported', verification: 'verified' },
            { id: 2, mediaType: 'ebook', language: 'rum', status: 'wanted', verification: '' },
            { id: 3, mediaType: 'audiobook', language: 'eng', status: 'review', verification: '' },
          ],
          legacyFiles: [
            { mediaType: 'ebook', language: '', status: 'imported', verification: 'unverified' },
          ],
        },
      ],
      total: 1,
      limit: 60,
      offset: 0,
    })

    render(<MemoryRouter><RequesterLibraryPage /></MemoryRouter>)

    expect(await screen.findByText('Romanian')).toBeInTheDocument()
    expect(screen.getByText('Review')).toBeInTheDocument()
    expect(screen.getByText('Unverified')).toBeInTheDocument()
    expect(screen.getByText('Language unknown')).toBeInTheDocument()
    expect(screen.queryByText('/private/book-alpha-eng.epub')).not.toBeInTheDocument()
  })
})

describe('RequestsPage', () => {
  beforeEach(() => vi.clearAllMocks())

  it('approves a book request with the choices from the inline form', async () => {
    vi.mocked(api.listRequestQueue).mockResolvedValue({ items: [req({ id: 9, mediaType: 'audiobook', username: 'reader' })], total: 1, limit: 50, offset: 0 })
    vi.mocked(api.approveRequest).mockResolvedValue(req({ id: 9, status: 'approved' }))
    const changed = vi.fn()
    window.addEventListener('bindery:requests-changed', changed)
    render(<RequestsPage />)

    fireEvent.click(await screen.findByRole('button', { name: 'Approve request for Dune' }))
    const form = screen.getByRole('form', { name: 'Approve Dune' })
    expect(within(form).getByRole('combobox', { name: 'Format' })).toHaveValue('audiobook')
    expect(within(form).queryByRole('combobox', { name: 'Root folder' })).not.toBeInTheDocument()
    fireEvent.click(within(form).getByRole('checkbox', { name: 'Search indexers after adding' }))
    fireEvent.click(within(form).getByRole('button', { name: 'Approve and add' }))

    await waitFor(() => expect(api.approveRequest).toHaveBeenCalledWith(9, { searchOnAdd: false, mediaType: 'audiobook' }))
    await waitFor(() => expect(screen.queryByText('Dune')).not.toBeInTheDocument())
    expect(changed).toHaveBeenCalled()
    window.removeEventListener('bindery:requests-changed', changed)
  })

  it('says who made a request on its owner\'s behalf', async () => {
    vi.mocked(api.listRequestQueue).mockResolvedValue({
      items: [
        req({ id: 1, title: 'By key', username: 'reader', createdVia: 'api_key' }),
        req({ id: 2, title: 'By admin', username: 'reader', createdVia: 'admin', createdBy: 'boss' }),
        req({ id: 3, title: 'Own', username: 'reader' }),
      ],
      total: 3, limit: 50, offset: 0,
    })
    render(<RequestsPage />)

    const row = async (title: string) => (await screen.findByText(title)).closest('li') as HTMLElement
    expect(within(await row('By key')).getByText(/made for them with the API key/)).toBeInTheDocument()
    expect(within(await row('By admin')).getByText(/made for them by boss/)).toBeInTheDocument()
    expect(within(await row('Own')).queryByText(/made for them/)).not.toBeInTheDocument()
  })

  it('offers the author choices prefilled from the instance defaults', async () => {
    vi.mocked(api.listRequestQueue).mockResolvedValue({ items: [req({ id: 5, kind: 'author', title: 'Frank Herbert' })], total: 1, limit: 50, offset: 0 })
    vi.mocked(api.approveRequest).mockResolvedValue(req({ id: 5, kind: 'author', status: 'approved' }))
    render(<RequestsPage />)

    fireEvent.click(await screen.findByRole('button', { name: 'Approve request for Frank Herbert' }))
    const form = screen.getByRole('form', { name: 'Approve Frank Herbert' })
    await waitFor(() => expect(within(form).getByRole('combobox', { name: 'Metadata profile' })).toHaveValue('1'))
    expect(within(form).getByRole('combobox', { name: 'Root folder' })).toHaveValue('4')
    fireEvent.click(within(form).getByRole('button', { name: 'Approve and add' }))

    await waitFor(() => expect(api.approveRequest).toHaveBeenCalledWith(5, expect.objectContaining({
      metadataProfileId: 1, rootFolderId: 4, monitorMode: 'all', mediaType: 'ebook', searchOnAdd: false,
    })))
  })

  it('declines with the reason typed', async () => {
    vi.mocked(api.listRequestQueue).mockResolvedValue({ items: [req({ id: 3 })], total: 1, limit: 50, offset: 0 })
    vi.mocked(api.declineRequest).mockResolvedValue(req({ id: 3, status: 'declined' }))
    render(<RequestsPage />)

    fireEvent.click(await screen.findByRole('button', { name: 'Decline request for Dune' }))
    const form = screen.getByRole('form', { name: 'Decline Dune' })
    fireEvent.change(within(form).getByRole('textbox', { name: 'Reason' }), { target: { value: '  Not in scope  ' } })
    fireEvent.click(within(form).getByRole('button', { name: 'Decline' }))

    await waitFor(() => expect(api.declineRequest).toHaveBeenCalledWith(3, 'Not in scope'))
  })

  it('titles the tab like every other page', async () => {
    vi.mocked(api.listRequestQueue).mockResolvedValue({ items: [], total: 0, limit: 50, offset: 0 })
    render(<RequestsPage />)

    await waitFor(() => expect(document.title).toBe('Requests \u00b7 Bindery'))
  })

  it('gives the empty queue a hint instead of a bare left aligned line', async () => {
    vi.mocked(api.listRequestQueue).mockResolvedValue({ items: [], total: 0, limit: 50, offset: 0 })
    render(<RequestsPage />)

    const empty = await screen.findByText('No requests are waiting.')
    expect(screen.getByText('New requests from your users show up here for you to approve or decline.')).toBeInTheDocument()
    expect(empty.parentElement).toHaveClass('text-center')
  })
})
