/*
 * Lucide icon node subset, v0.468.0.
 * Source: https://github.com/lucide-icons/lucide
 * License: ISC (see LICENSE in this directory).
 */

const NS = 'http://www.w3.org/2000/svg';

const ICONS = {
  sparkles: [['path',{d:'M9.937 15.5A2 2 0 0 0 8.5 14.063l-6.135-1.582a.5.5 0 0 1 0-.962L8.5 9.936A2 2 0 0 0 9.937 8.5l1.582-6.135a.5.5 0 0 1 .963 0L14.063 8.5A2 2 0 0 0 15.5 9.937l6.135 1.581a.5.5 0 0 1 0 .964L15.5 14.063a2 2 0 0 0-1.437 1.437l-1.582 6.135a.5.5 0 0 1-.963 0z'}],['path',{d:'M20 3v4'}],['path',{d:'M22 5h-4'}],['path',{d:'M4 17v2'}],['path',{d:'M5 18H3'}]],
  images: [['path',{d:'M18 22H4a2 2 0 0 1-2-2V6'}],['path',{d:'m22 13-1.296-1.296a2.41 2.41 0 0 0-3.408 0L11 18'}],['circle',{cx:'12',cy:'8',r:'2'}],['rect',{width:'16',height:'16',x:'6',y:'2',rx:'2'}]],
  'layout-template': [['rect',{width:'18',height:'7',x:'3',y:'3',rx:'1'}],['rect',{width:'9',height:'7',x:'3',y:'14',rx:'1'}],['rect',{width:'5',height:'7',x:'16',y:'14',rx:'1'}]],
  plug: [['path',{d:'M12 22v-5'}],['path',{d:'M9 8V2'}],['path',{d:'M15 8V2'}],['path',{d:'M18 8v5a4 4 0 0 1-4 4h-4a4 4 0 0 1-4-4V8Z'}]],
  settings: [['path',{d:'M12.22 2h-.44a2 2 0 0 0-2 2v.18a2 2 0 0 1-1 1.73l-.43.25a2 2 0 0 1-2 0l-.15-.08a2 2 0 0 0-2.73.73l-.22.38a2 2 0 0 0 .73 2.73l.15.1a2 2 0 0 1 1 1.72v.51a2 2 0 0 1-1 1.74l-.15.09a2 2 0 0 0-.73 2.73l.22.38a2 2 0 0 0 2.73.73l.15-.08a2 2 0 0 1 2 0l.43.25a2 2 0 0 1 1 1.73V20a2 2 0 0 0 2 2h.44a2 2 0 0 0 2-2v-.18a2 2 0 0 1 1-1.73l.43-.25a2 2 0 0 1 2 0l.15.08a2 2 0 0 0 2.73-.73l.22-.39a2 2 0 0 0-.73-2.73l-.15-.08a2 2 0 0 1-1-1.74v-.5a2 2 0 0 1 1-1.74l.15-.09a2 2 0 0 0 .73-2.73l-.22-.38a2 2 0 0 0-2.73-.73l-.15.08a2 2 0 0 1-2 0l-.43-.25a2 2 0 0 1-1-1.73V4a2 2 0 0 0-2-2z'}],['circle',{cx:'12',cy:'12',r:'3'}]],
  plus: [['path',{d:'M5 12h14'}],['path',{d:'M12 5v14'}]],
  'log-out': [['path',{d:'M9 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4'}],['polyline',{points:'16 17 21 12 16 7'}],['line',{x1:'21',x2:'9',y1:'12',y2:'12'}]],
  'user-round': [['circle',{cx:'12',cy:'8',r:'5'}],['path',{d:'M20 21a8 8 0 0 0-16 0'}]],
  'circle-help': [['circle',{cx:'12',cy:'12',r:'10'}],['path',{d:'M9.09 9a3 3 0 0 1 5.83 1c0 2-3 3-3 3'}],['path',{d:'M12 17h.01'}]],
  menu: [['line',{x1:'4',x2:'20',y1:'12',y2:'12'}],['line',{x1:'4',x2:'20',y1:'6',y2:'6'}],['line',{x1:'4',x2:'20',y1:'18',y2:'18'}]],
  x: [['path',{d:'M18 6 6 18'}],['path',{d:'m6 6 12 12'}]],
  eye: [['path',{d:'M2.062 12.348a1 1 0 0 1 0-.696 10.75 10.75 0 0 1 19.876 0 1 1 0 0 1 0 .696 10.75 10.75 0 0 1-19.876 0'}],['circle',{cx:'12',cy:'12',r:'3'}]],
  'eye-off': [['path',{d:'M10.733 5.076a10.744 10.744 0 0 1 11.205 6.575 1 1 0 0 1 0 .696 10.747 10.747 0 0 1-1.444 2.49'}],['path',{d:'M14.084 14.158a3 3 0 0 1-4.242-4.242'}],['path',{d:'M17.479 17.499a10.75 10.75 0 0 1-15.417-5.151 1 1 0 0 1 0-.696 10.75 10.75 0 0 1 4.446-5.143'}],['path',{d:'m2 2 20 20'}]],
  upload: [['path',{d:'M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4'}],['polyline',{points:'17 8 12 3 7 8'}],['line',{x1:'12',x2:'12',y1:'3',y2:'15'}]],
  image: [['rect',{width:'18',height:'18',x:'3',y:'3',rx:'2',ry:'2'}],['circle',{cx:'9',cy:'9',r:'2'}],['path',{d:'m21 15-3.086-3.086a2 2 0 0 0-2.828 0L6 21'}]],
  'wand-sparkles': [['path',{d:'m21.64 3.64-1.28-1.28a1.21 1.21 0 0 0-1.72 0L2.36 18.64a1.21 1.21 0 0 0 0 1.72l1.28 1.28a1.2 1.2 0 0 0 1.72 0L21.64 5.36a1.2 1.2 0 0 0 0-1.72'}],['path',{d:'m14 7 3 3'}],['path',{d:'M5 6v4'}],['path',{d:'M19 14v4'}],['path',{d:'M10 2v2'}],['path',{d:'M7 8H3'}],['path',{d:'M21 16h-4'}],['path',{d:'M11 3H9'}]],
  download: [['path',{d:'M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4'}],['polyline',{points:'7 10 12 15 17 10'}],['line',{x1:'12',x2:'12',y1:'15',y2:'3'}]],
  'rotate-ccw': [['path',{d:'M3 12a9 9 0 1 0 9-9 9.75 9.75 0 0 0-6.74 2.74L3 8'}],['path',{d:'M3 3v5h5'}]],
  clock: [['circle',{cx:'12',cy:'12',r:'10'}],['polyline',{points:'12 6 12 12 16 14'}]],
  'circle-check': [['circle',{cx:'12',cy:'12',r:'10'}],['path',{d:'m9 12 2 2 4-4'}]],
  'circle-alert': [['circle',{cx:'12',cy:'12',r:'10'}],['line',{x1:'12',x2:'12',y1:'8',y2:'12'}],['line',{x1:'12',x2:'12.01',y1:'16',y2:'16'}]],
  'loader-circle': [['path',{d:'M21 12a9 9 0 1 1-6.219-8.56'}]],
  search: [['circle',{cx:'11',cy:'11',r:'8'}],['path',{d:'m21 21-4.3-4.3'}]],
  'refresh-cw': [['path',{d:'M3 12a9 9 0 0 1 9-9 9.75 9.75 0 0 1 6.74 2.74L21 8'}],['path',{d:'M21 3v5h-5'}],['path',{d:'M21 12a9 9 0 0 1-9 9 9.75 9.75 0 0 1-6.74-2.74L3 16'}],['path',{d:'M8 16H3v5'}]],
  'trash-2': [['path',{d:'M3 6h18'}],['path',{d:'M19 6v14c0 1-1 2-2 2H7c-1 0-2-1-2-2V6'}],['path',{d:'M8 6V4c0-1 1-2 2-2h4c1 0 2 1 2 2v2'}],['line',{x1:'10',x2:'10',y1:'11',y2:'17'}],['line',{x1:'14',x2:'14',y1:'11',y2:'17'}]],
  copy: [['rect',{width:'14',height:'14',x:'8',y:'8',rx:'2',ry:'2'}],['path',{d:'M4 16c-1.1 0-2-.9-2-2V4c0-1.1.9-2 2-2h10c1.1 0 2 .9 2 2'}]],
  'key-round': [['path',{d:'M2.586 17.414A2 2 0 0 0 2 18.828V21a1 1 0 0 0 1 1h3a1 1 0 0 0 1-1v-1a1 1 0 0 1 1-1h1a1 1 0 0 0 1-1v-1a1 1 0 0 1 1-1h.172a2 2 0 0 0 1.414-.586l.814-.814a6.5 6.5 0 1 0-4-4z'}],['circle',{cx:'16.5',cy:'7.5',r:'.5',fill:'currentColor'}]],
  code: [['polyline',{points:'16 18 22 12 16 6'}],['polyline',{points:'8 6 2 12 8 18'}]],
  save: [['path',{d:'M15.2 3a2 2 0 0 1 1.4.6l3.8 3.8a2 2 0 0 1 .6 1.4V19a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2z'}],['path',{d:'M17 21v-7a1 1 0 0 0-1-1H8a1 1 0 0 0-1 1v7'}],['path',{d:'M7 3v4a1 1 0 0 0 1 1h7'}]],
  'chevron-down': [['path',{d:'m6 9 6 6 6-6'}]],
  'chevron-right': [['path',{d:'m9 18 6-6-6-6'}]],
  'file-json': [['path',{d:'M15 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V7Z'}],['path',{d:'M14 2v4a2 2 0 0 0 2 2h4'}],['path',{d:'M10 12a1 1 0 0 0-1 1v1a1 1 0 0 1-1 1 1 1 0 0 1 1 1v1a1 1 0 0 0 1 1'}],['path',{d:'M14 18a1 1 0 0 0 1-1v-1a1 1 0 0 1 1-1 1 1 0 0 1-1-1v-1a1 1 0 0 0-1-1'}]],
  'folder-open': [['path',{d:'m6 14 1.5-2.9A2 2 0 0 1 9.24 10H20a2 2 0 0 1 1.94 2.5l-1.54 6a2 2 0 0 1-1.95 1.5H4a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h3.9a2 2 0 0 1 1.69.9l.81 1.2a2 2 0 0 0 1.67.9H18a2 2 0 0 1 2 2v2'}]],
  'sliders-horizontal': [['line',{x1:'21',x2:'14',y1:'4',y2:'4'}],['line',{x1:'10',x2:'3',y1:'4',y2:'4'}],['line',{x1:'21',x2:'12',y1:'12',y2:'12'}],['line',{x1:'8',x2:'3',y1:'12',y2:'12'}],['line',{x1:'21',x2:'16',y1:'20',y2:'20'}],['line',{x1:'12',x2:'3',y1:'20',y2:'20'}],['line',{x1:'14',x2:'14',y1:'2',y2:'6'}],['line',{x1:'8',x2:'8',y1:'10',y2:'14'}],['line',{x1:'16',x2:'16',y1:'18',y2:'22'}]],
  lock: [['rect',{width:'18',height:'11',x:'3',y:'11',rx:'2',ry:'2'}],['path',{d:'M7 11V7a5 5 0 0 1 10 0v4'}]],
  'shield-alert': [['path',{d:'M20 13c0 5-3.5 7.5-7.66 8.95a1 1 0 0 1-.67-.01C7.5 20.5 4 18 4 13V6a1 1 0 0 1 1-1c2 0 4.5-1.2 6.24-2.72a1.17 1.17 0 0 1 1.52 0C14.51 3.81 17 5 19 5a1 1 0 0 1 1 1z'}],['path',{d:'M12 8v4'}],['path',{d:'M12 16h.01'}]],
  'shield-check': [['path',{d:'M20 13c0 5-3.5 7.5-7.66 8.95a1 1 0 0 1-.67-.01C7.5 20.5 4 18 4 13V6a1 1 0 0 1 1-1c2 0 4.5-1.2 6.24-2.72a1.17 1.17 0 0 1 1.52 0C14.51 3.81 17 5 19 5a1 1 0 0 1 1 1z'}],['path',{d:'m9 12 2 2 4-4'}]],
  'circle-plus': [['circle',{cx:'12',cy:'12',r:'10'}],['path',{d:'M8 12h8'}],['path',{d:'M12 8v8'}]],
  'square-pen': [['path',{d:'M12 3H5a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h14a2 2 0 0 0 2-2v-7'}],['path',{d:'M18.375 2.625a1 1 0 0 1 3 3l-9.013 9.014a2 2 0 0 1-.853.505l-2.873.84a.5.5 0 0 1-.62-.62l.84-2.873a2 2 0 0 1 .506-.852z'}]],
  list: [['path',{d:'M3 12h.01'}],['path',{d:'M3 18h.01'}],['path',{d:'M3 6h.01'}],['path',{d:'M8 12h13'}],['path',{d:'M8 18h13'}],['path',{d:'M8 6h13'}]],
  filter: [['polygon',{points:'22 3 2 3 10 12.46 10 19 14 21 14 12.46 22 3'}]],
  'grid-2x2': [['path',{d:'M12 3v18'}],['path',{d:'M3 12h18'}],['rect',{x:'3',y:'3',width:'18',height:'18',rx:'2'}]],
  'external-link': [['path',{d:'M15 3h6v6'}],['path',{d:'M10 14 21 3'}],['path',{d:'M18 13v6a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V8a2 2 0 0 1 2-2h6'}]],
  play: [['polygon',{points:'6 3 20 12 6 21 6 3'}]],
  'circle-x': [['circle',{cx:'12',cy:'12',r:'10'}],['path',{d:'m15 9-6 6'}],['path',{d:'m9 9 6 6'}]],
  info: [['circle',{cx:'12',cy:'12',r:'10'}],['path',{d:'M12 16v-4'}],['path',{d:'M12 8h.01'}]],
  'monitor-smartphone': [['path',{d:'M18 8V6a2 2 0 0 0-2-2H4a2 2 0 0 0-2 2v7a2 2 0 0 0 2 2h8'}],['path',{d:'M10 19v-3.96 3.15'}],['path',{d:'M7 19h5'}],['rect',{width:'6',height:'10',x:'16',y:'12',rx:'2'}]],
  table: [['path',{d:'M12 3v18'}],['rect',{width:'18',height:'18',x:'3',y:'3',rx:'2'}],['path',{d:'M3 9h18'}],['path',{d:'M3 15h18'}]],
  archive: [['rect',{width:'20',height:'5',x:'2',y:'3',rx:'1'}],['path',{d:'M4 8v11a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8'}],['path',{d:'M10 12h4'}]],
  ellipsis: [['circle',{cx:'12',cy:'12',r:'1'}],['circle',{cx:'19',cy:'12',r:'1'}],['circle',{cx:'5',cy:'12',r:'1'}]],
  'arrow-left': [['path',{d:'m12 19-7-7 7-7'}],['path',{d:'M19 12H5'}]],
  'arrow-right': [['path',{d:'M5 12h14'}],['path',{d:'m12 5 7 7-7 7'}]],
  check: [['path',{d:'M20 6 9 17l-5-5'}]],
  package: [['path',{d:'M11 21.73a2 2 0 0 0 2 0l7-4A2 2 0 0 0 21 16V8a2 2 0 0 0-1-1.73l-7-4a2 2 0 0 0-2 0l-7 4A2 2 0 0 0 3 8v8a2 2 0 0 0 1 1.73z'}],['path',{d:'M12 22V12'}],['path',{d:'m3.3 7 7.703 4.734a2 2 0 0 0 1.994 0L20.7 7'}],['path',{d:'m7.5 4.27 9 5.15'}]],
  'image-plus': [['path',{d:'M16 5h6'}],['path',{d:'M19 2v6'}],['path',{d:'M21 11.5V19a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h7.5'}],['path',{d:'m21 15-3.086-3.086a2 2 0 0 0-2.828 0L6 21'}],['circle',{cx:'9',cy:'9',r:'2'}]],
  'book-open': [['path',{d:'M12 7v14'}],['path',{d:'M3 18a1 1 0 0 1-1-1V4a1 1 0 0 1 1-1h5a4 4 0 0 1 4 4 4 4 0 0 1 4-4h5a1 1 0 0 1 1 1v13a1 1 0 0 1-1 1h-6a3 3 0 0 0-3 3 3 3 0 0 0-3-3z'}]],
  'folder-tree': [['path',{d:'M20 10a1 1 0 0 0 1-1V6a1 1 0 0 0-1-1h-2.5a1 1 0 0 1-.8-.4l-.9-1.2A1 1 0 0 0 15 3h-2a1 1 0 0 0-1 1v5a1 1 0 0 0 1 1Z'}],['path',{d:'M20 21a1 1 0 0 0 1-1v-3a1 1 0 0 0-1-1h-2.9a1 1 0 0 1-.88-.55l-.42-.85a1 1 0 0 0-.92-.6H13a1 1 0 0 0-1 1v5a1 1 0 0 0 1 1Z'}],['path',{d:'M3 5a2 2 0 0 0 2 2h3'}],['path',{d:'M3 3v13a2 2 0 0 0 2 2h3'}]],
  'layout-grid': [['rect',{width:'7',height:'7',x:'3',y:'3',rx:'1'}],['rect',{width:'7',height:'7',x:'14',y:'3',rx:'1'}],['rect',{width:'7',height:'7',x:'14',y:'14',rx:'1'}],['rect',{width:'7',height:'7',x:'3',y:'14',rx:'1'}]],
  pencil: [['path',{d:'M21.174 6.812a1 1 0 0 0-3.986-3.987L3.842 16.174a2 2 0 0 0-.5.83l-1.321 4.352a.5.5 0 0 0 .623.622l4.353-1.32a2 2 0 0 0 .83-.497z'}],['path',{d:'m15 5 4 4'}]],
  palette: [['circle',{cx:'13.5',cy:'6.5',r:'.5',fill:'currentColor'}],['circle',{cx:'17.5',cy:'10.5',r:'.5',fill:'currentColor'}],['circle',{cx:'8.5',cy:'7.5',r:'.5',fill:'currentColor'}],['circle',{cx:'6.5',cy:'12.5',r:'.5',fill:'currentColor'}],['path',{d:'M12 2C6.5 2 2 6.5 2 12s4.5 10 10 10c.926 0 1.648-.746 1.648-1.688 0-.437-.18-.835-.437-1.125-.29-.289-.438-.652-.438-1.125a1.64 1.64 0 0 1 1.668-1.668h1.996c3.051 0 5.555-2.503 5.555-5.554C21.965 6.012 17.461 2 12 2z'}]],
};

const ALIASES = {
  'check-circle-2': 'circle-check',
  'alert-circle': 'circle-alert',
  'x-circle': 'circle-x',
  'rotate-cw': 'refresh-cw',
  'upload-cloud': 'upload',
  'code-2': 'code',
  'grid': 'grid-2x2',
  'edit': 'square-pen',
  'trash': 'trash-2',
  'key': 'key-round',
  'help-circle': 'circle-help',
};

export function createLucideIcon(name, { size = 20, label = '', className = '' } = {}) {
  const resolved = ALIASES[name] || name;
  const nodes = ICONS[resolved] || ICONS['circle-help'];
  const svg = document.createElementNS(NS, 'svg');
  svg.setAttribute('viewBox', '0 0 24 24');
  svg.setAttribute('width', String(size));
  svg.setAttribute('height', String(size));
  svg.setAttribute('fill', 'none');
  svg.setAttribute('stroke', 'currentColor');
  svg.setAttribute('stroke-width', '1.75');
  svg.setAttribute('stroke-linecap', 'round');
  svg.setAttribute('stroke-linejoin', 'round');
  svg.setAttribute('focusable', 'false');
  svg.classList.add('dk-icon');
  if (className) svg.classList.add(...className.split(/\s+/).filter(Boolean));
  if (label) {
    svg.setAttribute('role', 'img');
    svg.setAttribute('aria-label', label);
  } else {
    svg.setAttribute('aria-hidden', 'true');
  }
  for (const [tag, attrs] of nodes) {
    const child = document.createElementNS(NS, tag);
    for (const [key, value] of Object.entries(attrs)) child.setAttribute(key, String(value));
    svg.append(child);
  }
  return svg;
}

export const LUCIDE_VERSION = '0.468.0';
