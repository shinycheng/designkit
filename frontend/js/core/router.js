export function visibleRoutes(routes, user) {
  return routes.filter((route) => !route.adminOnly || user?.role === 'admin');
}

export function resolveRoute(routes, user, hash = window.location.hash) {
  const visible = visibleRoutes(routes, user);
  return visible.find((route) => route.hash === hash) || visible[0];
}

export function navigate(hash, { replace = false } = {}) {
  if (replace) history.replaceState(null, '', hash);
  else window.location.hash = hash;
}
