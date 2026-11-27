import { findRoute, routeGuideTitle } from '../routes'

// pageGuide returns the "What's here" introduction for a signed-in route,
// from the route registry (dashboard-06).
export function pageGuide(route, { pageTitle, authMode } = {}) {
  if (pageTitle === 'Access denied') return {
    title: 'Access denied',
    description: 'Your current role cannot open this tab. '
      + 'An app administrator can review the access you need.',
  }
  const match = findRoute(route)
  if (!match || pageTitle === 'Not found') return {
    title: 'Page not found',
    description: 'This address does not match a page. '
      + 'Choose a tab in the navigation to return to your database workspace.',
  }
  const description = authMode === 'databricks_app' && match.platformGuide
    ? match.platformGuide : match.guide
  return { title: routeGuideTitle(match), description }
}
