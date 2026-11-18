// NeedsOneDatabase stands in for a page that cannot read a header tag scope
// (ux-10b): it says so, offers the whole fleet, and links the tag's
// databases on the Fleet page (admin only), where one can be chosen.
export function NeedsOneDatabase({ tag, onShowAll, isAdmin }) {
  return (
    <div data-testid="needs-one-database" className="rounded p-5 space-y-3"
      style={{ background: 'var(--bg-card)', border: '1px solid var(--border)' }}>
      <h2 className="text-lg font-semibold" style={{ color: 'var(--text-primary)' }}>
        This page needs one database
      </h2>
      <p className="text-sm" style={{ color: 'var(--text-secondary)' }}>
        The scope is the databases tagged <span className="font-mono">{tag}</span>, and
        this page cannot be filtered by tag. Choose one database in the scope picker, or
        show the whole fleet.
      </p>
      <div className="flex flex-wrap gap-3 text-sm">
        <button type="button" onClick={onShowAll} className="rounded px-3 py-1.5"
          style={{ background: 'var(--accent)', color: '#fff' }}>
          Show all databases
        </button>
        {isAdmin && (
          <a href={`#/manage-databases?tag=${encodeURIComponent(tag)}`} className="underline"
            style={{ color: 'var(--accent)' }}>
            List the tagged databases
          </a>
        )}
      </div>
    </div>
  )
}
