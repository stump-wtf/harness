/* @ds-bundle: {"format":4,"namespace":"BubbleteaTUIDesignSystem_5c2f37","components":[{"name":"Badge","sourcePath":"components/display/Badge.jsx"},{"name":"List","sourcePath":"components/display/List.jsx"},{"name":"Table","sourcePath":"components/display/Table.jsx"},{"name":"Tabs","sourcePath":"components/display/Tabs.jsx"},{"name":"Dialog","sourcePath":"components/feedback/Dialog.jsx"},{"name":"KeyHint","sourcePath":"components/feedback/KeyHint.jsx"},{"name":"Progress","sourcePath":"components/feedback/Progress.jsx"},{"name":"Spinner","sourcePath":"components/feedback/Spinner.jsx"},{"name":"Button","sourcePath":"components/forms/Button.jsx"},{"name":"Checkbox","sourcePath":"components/forms/Checkbox.jsx"},{"name":"TextInput","sourcePath":"components/forms/TextInput.jsx"},{"name":"Toggle","sourcePath":"components/forms/Toggle.jsx"},{"name":"Kbd","sourcePath":"components/terminal/Kbd.jsx"},{"name":"StatusBar","sourcePath":"components/terminal/StatusBar.jsx"},{"name":"TerminalWindow","sourcePath":"components/terminal/TerminalWindow.jsx"}],"sourceHashes":{"components/display/Badge.jsx":"5954c33b61e8","components/display/List.jsx":"da1011001194","components/display/Table.jsx":"a3ecafcab3cd","components/display/Tabs.jsx":"210ce81b4e29","components/feedback/Dialog.jsx":"6ba2dc56f0bd","components/feedback/KeyHint.jsx":"cef947ab0535","components/feedback/Progress.jsx":"5b670091b932","components/feedback/Spinner.jsx":"ac0011a209b4","components/forms/Button.jsx":"ae1e3793e0f8","components/forms/Checkbox.jsx":"fece4a56f64c","components/forms/TextInput.jsx":"89c2833794df","components/forms/Toggle.jsx":"6e2313aa1445","components/terminal/Kbd.jsx":"49d37093483b","components/terminal/StatusBar.jsx":"c52056757808","components/terminal/TerminalWindow.jsx":"3f95b2804ae1","ui_kits/charm-cli/App.export.jsx":"f7bf735c619f","ui_kits/charm-cli/App.jsx":"ccac94e7e985","ui_kits/charm-cli/FormScreen.jsx":"696b0130a16d","ui_kits/charm-cli/InstallScreen.jsx":"a762abf2913d","ui_kits/charm-cli/MenuScreen.jsx":"672ad0598166","ui_kits/glow/GlowReader.jsx":"12fb4fb043d0"},"inlinedExternals":[],"unexposedExports":[]} */

(() => {

const __ds_ns = (window.BubbleteaTUIDesignSystem_5c2f37 = window.BubbleteaTUIDesignSystem_5c2f37 || {});

const __ds_scope = {};

(__ds_ns.__errors = __ds_ns.__errors || []);

// components/display/Badge.jsx
try { (() => {
function _extends() { return _extends = Object.assign ? Object.assign.bind() : function (n) { for (var e = 1; e < arguments.length; e++) { var t = arguments[e]; for (var r in t) ({}).hasOwnProperty.call(t, r) && (n[r] = t[r]); } return n; }, _extends.apply(null, arguments); }
/**
 * Badge — a Lip Gloss label pill. Solid neon fill or subtle tinted outline,
 * with an optional leading status dot.
 */
function Badge({
  children,
  tone = 'primary',
  variant = 'solid',
  dot = false,
  style,
  ...rest
}) {
  const tones = {
    primary: 'var(--charm-purple)',
    accent: 'var(--charm-pink)',
    info: 'var(--neon-cyan)',
    success: 'var(--neon-mint)',
    warning: 'var(--neon-gold)',
    danger: 'var(--neon-coral)',
    muted: 'var(--text-muted)'
  };
  const c = tones[tone] || tones.primary;
  const solid = variant === 'solid';
  return /*#__PURE__*/React.createElement("span", _extends({
    style: {
      display: 'inline-flex',
      alignItems: 'center',
      gap: 6,
      padding: '2px 8px',
      borderRadius: 0,
      fontFamily: 'var(--font-mono)',
      fontSize: 'var(--text-2xs)',
      fontWeight: 700,
      letterSpacing: '0.06em',
      textTransform: 'uppercase',
      lineHeight: 1.6,
      color: solid ? 'var(--text-on-accent)' : c,
      background: solid ? c : 'color-mix(in oklab, ' + 'transparent 86%, ' + c + ')',
      border: solid ? 'none' : `1px solid ${c}`,
      ...style
    }
  }, rest), dot && /*#__PURE__*/React.createElement("span", {
    style: {
      width: 6,
      height: 6,
      borderRadius: '50%',
      background: solid ? 'var(--text-on-accent)' : c,
      boxShadow: solid ? 'none' : `0 0 6px ${c}`
    }
  }), children);
}
Object.assign(__ds_scope, { Badge });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/display/Badge.jsx", error: String((e && e.message) || e) }); }

// components/display/List.jsx
try { (() => {
function _extends() { return _extends = Object.assign ? Object.assign.bind() : function (n) { for (var e = 1; e < arguments.length; e++) { var t = arguments[e]; for (var r in t) ({}).hasOwnProperty.call(t, r) && (n[r] = t[r]); } return n; }, _extends.apply(null, arguments); }
/**
 * List — the Bubbles list. A stack of selectable rows; the selected row
 * shows the '›' cursor, a pink rail, and a highlight. Optional two-line
 * items with a dim description (Bubbles' default delegate).
 */
function List({
  items = [],
  selected = 0,
  dense = false,
  style,
  ...rest
}) {
  return /*#__PURE__*/React.createElement("div", _extends({
    style: {
      display: 'flex',
      flexDirection: 'column',
      gap: dense ? 0 : 2,
      fontFamily: 'var(--font-mono)',
      ...style
    }
  }, rest), items.map((it, i) => {
    const item = typeof it === 'string' ? {
      title: it
    } : it;
    const isSel = i === selected;
    return /*#__PURE__*/React.createElement("div", {
      key: i,
      style: {
        display: 'flex',
        gap: 8,
        alignItems: 'flex-start',
        padding: dense ? '3px 12px' : '6px 12px',
        borderRadius: 'var(--radius-sm)',
        background: isSel ? 'var(--tint-accent)' : 'transparent',
        boxShadow: isSel ? 'inset 3px 0 0 var(--charm-pink)' : 'none'
      }
    }, /*#__PURE__*/React.createElement("span", {
      style: {
        color: 'var(--charm-pink)',
        fontWeight: 700,
        width: '1ch',
        flexShrink: 0
      }
    }, isSel ? '›' : ' '), /*#__PURE__*/React.createElement("div", {
      style: {
        display: 'flex',
        flexDirection: 'column',
        gap: 1,
        minWidth: 0
      }
    }, /*#__PURE__*/React.createElement("span", {
      style: {
        fontSize: 'var(--text-sm)',
        fontWeight: isSel ? 700 : 400,
        color: isSel ? 'var(--text-bright)' : 'var(--text-body)',
        whiteSpace: 'nowrap',
        overflow: 'hidden',
        textOverflow: 'ellipsis'
      }
    }, item.title), !dense && item.desc && /*#__PURE__*/React.createElement("span", {
      style: {
        fontSize: 'var(--text-xs)',
        color: isSel ? 'var(--text-muted)' : 'var(--text-dim)',
        whiteSpace: 'nowrap',
        overflow: 'hidden',
        textOverflow: 'ellipsis'
      }
    }, item.desc)), item.badge && /*#__PURE__*/React.createElement("span", {
      style: {
        marginLeft: 'auto',
        fontSize: 'var(--text-2xs)',
        color: 'var(--neon-cyan)',
        flexShrink: 0
      }
    }, item.badge));
  }));
}
Object.assign(__ds_scope, { List });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/display/List.jsx", error: String((e && e.message) || e) }); }

// components/display/Table.jsx
try { (() => {
function _extends() { return _extends = Object.assign ? Object.assign.bind() : function (n) { for (var e = 1; e < arguments.length; e++) { var t = arguments[e]; for (var r in t) ({}).hasOwnProperty.call(t, r) && (n[r] = t[r]); } return n; }, _extends.apply(null, arguments); }
/**
 * Table — a Lip Gloss table. Rounded outer border, a purple-tinted header
 * row, hairline row separators, and an optional highlighted selected row.
 */
function Table({
  columns = [],
  rows = [],
  selected = -1,
  style,
  ...rest
}) {
  const grid = columns.map(c => c.width || '1fr').join(' ');
  return /*#__PURE__*/React.createElement("div", _extends({
    style: {
      border: '1.5px solid var(--line)',
      borderRadius: 'var(--radius-sm)',
      overflow: 'hidden',
      fontFamily: 'var(--font-mono)',
      fontSize: 'var(--text-sm)',
      background: 'var(--bg-terminal)',
      ...style
    }
  }, rest), /*#__PURE__*/React.createElement("div", {
    style: {
      display: 'grid',
      gridTemplateColumns: grid,
      gap: 0,
      background: 'var(--tint-primary)',
      borderBottom: '1px solid var(--line)'
    }
  }, columns.map((c, i) => /*#__PURE__*/React.createElement("span", {
    key: i,
    style: {
      padding: '8px 14px',
      fontWeight: 700,
      fontSize: 'var(--text-xs)',
      letterSpacing: '0.06em',
      textTransform: 'uppercase',
      color: 'var(--neon-lilac)',
      textAlign: c.align || 'left'
    }
  }, c.label))), rows.map((row, ri) => {
    const isSel = ri === selected;
    return /*#__PURE__*/React.createElement("div", {
      key: ri,
      style: {
        display: 'grid',
        gridTemplateColumns: grid,
        borderBottom: ri < rows.length - 1 ? '1px solid var(--line-dim)' : 'none',
        background: isSel ? 'var(--tint-info)' : 'transparent',
        boxShadow: isSel ? 'inset 3px 0 0 var(--neon-cyan)' : 'none'
      }
    }, columns.map((c, ci) => /*#__PURE__*/React.createElement("span", {
      key: ci,
      style: {
        padding: '7px 14px',
        textAlign: c.align || 'left',
        color: isSel ? 'var(--text-bright)' : ci === 0 ? 'var(--text-body)' : 'var(--text-muted)',
        whiteSpace: 'nowrap',
        overflow: 'hidden',
        textOverflow: 'ellipsis'
      }
    }, row[c.key])));
  }));
}
Object.assign(__ds_scope, { Table });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/display/Table.jsx", error: String((e && e.message) || e) }); }

// components/display/Tabs.jsx
try { (() => {
function _extends() { return _extends = Object.assign ? Object.assign.bind() : function (n) { for (var e = 1; e < arguments.length; e++) { var t = arguments[e]; for (var r in t) ({}).hasOwnProperty.call(t, r) && (n[r] = t[r]); } return n; }, _extends.apply(null, arguments); }
/**
 * Tabs — the Lip Gloss tabs example. A row of soft-square tabs; the active
 * one is lifted, bright, and underlit with the brand gradient.
 */
function Tabs({
  tabs = [],
  active = 0,
  style,
  ...rest
}) {
  return /*#__PURE__*/React.createElement("div", _extends({
    style: {
      display: 'flex',
      gap: 6,
      alignItems: 'flex-end',
      fontFamily: 'var(--font-mono)',
      ...style
    }
  }, rest), tabs.map((t, i) => {
    const isActive = i === active;
    const label = typeof t === 'string' ? t : t.label;
    return /*#__PURE__*/React.createElement("div", {
      key: i,
      style: {
        padding: '6px 16px',
        fontSize: 'var(--text-sm)',
        fontWeight: isActive ? 700 : 400,
        color: isActive ? 'var(--text-bright)' : 'var(--text-muted)',
        background: isActive ? 'var(--bg-surface-2)' : 'transparent',
        border: `1px solid ${isActive ? 'var(--charm-purple)' : 'var(--line-dim)'}`,
        borderBottom: 'none',
        borderRadius: 'var(--radius-sm) var(--radius-sm) 0 0',
        position: 'relative',
        cursor: 'pointer',
        whiteSpace: 'nowrap',
        transition: 'color var(--dur-base) var(--ease-out), background-color var(--dur-base) var(--ease-out), border-color var(--dur-base) var(--ease-out), box-shadow var(--dur-base) var(--ease-out)'
      }
    }, label);
  }));
}
Object.assign(__ds_scope, { Tabs });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/display/Tabs.jsx", error: String((e && e.message) || e) }); }

// components/feedback/Dialog.jsx
try { (() => {
function _extends() { return _extends = Object.assign ? Object.assign.bind() : function (n) { for (var e = 1; e < arguments.length; e++) { var t = arguments[e]; for (var r in t) ({}).hasOwnProperty.call(t, r) && (n[r] = t[r]); } return n; }, _extends.apply(null, arguments); }
/**
 * Dialog — a centered modal box (Huh? confirm / Gum confirm). Rounded neon
 * border, title, message, and an action row. Render `actions` yourself with
 * <Button> children so focus states stay in your control.
 */
function Dialog({
  title,
  icon,
  children,
  actions,
  tone = 'primary',
  width = 380,
  style,
  ...rest
}) {
  const tones = {
    primary: 'var(--charm-purple)',
    accent: 'var(--charm-pink)',
    danger: 'var(--neon-coral)',
    info: 'var(--neon-cyan)',
    success: 'var(--neon-mint)'
  };
  const c = tones[tone] || tones.primary;
  return /*#__PURE__*/React.createElement("div", _extends({
    style: {
      width,
      background: 'var(--bg-surface)',
      border: `1.5px solid ${c}`,
      borderRadius: 'var(--radius-sm)',
      fontFamily: 'var(--font-mono)',
      overflow: 'hidden',
      ...style
    },
    role: "dialog",
    "aria-modal": "true"
  }, rest), title && /*#__PURE__*/React.createElement("div", {
    style: {
      display: 'flex',
      alignItems: 'center',
      gap: 10,
      padding: '12px 18px',
      borderBottom: '1px solid var(--line-dim)',
      background: `color-mix(in oklab, transparent 88%, ${c})`
    }
  }, icon && /*#__PURE__*/React.createElement("span", {
    style: {
      color: c,
      fontSize: 'var(--text-md)'
    }
  }, icon), /*#__PURE__*/React.createElement("span", {
    style: {
      fontWeight: 700,
      fontSize: 'var(--text-sm)',
      color: 'var(--text-bright)',
      letterSpacing: '0.02em'
    }
  }, title)), /*#__PURE__*/React.createElement("div", {
    style: {
      padding: '16px 18px',
      fontSize: 'var(--text-sm)',
      color: 'var(--text-body)',
      lineHeight: 'var(--leading-loose)'
    }
  }, children), actions && /*#__PURE__*/React.createElement("div", {
    style: {
      display: 'flex',
      justifyContent: 'flex-end',
      gap: 10,
      padding: '12px 18px',
      borderTop: '1px solid var(--line-dim)'
    }
  }, actions));
}
Object.assign(__ds_scope, { Dialog });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/feedback/Dialog.jsx", error: String((e && e.message) || e) }); }

// components/feedback/KeyHint.jsx
try { (() => {
function _extends() { return _extends = Object.assign ? Object.assign.bind() : function (n) { for (var e = 1; e < arguments.length; e++) { var t = arguments[e]; for (var r in t) ({}).hasOwnProperty.call(t, r) && (n[r] = t[r]); } return n; }, _extends.apply(null, arguments); }
/**
 * KeyHint — the Bubbles help bar. A dim row of "key action" pairs joined by
 * bullets, exactly like the footer help every Charm app renders.
 */
function KeyHint({
  hints = [],
  style,
  ...rest
}) {
  return /*#__PURE__*/React.createElement("div", _extends({
    style: {
      display: 'flex',
      flexWrap: 'wrap',
      alignItems: 'center',
      gap: '4px 4px',
      fontFamily: 'var(--font-mono)',
      fontSize: 'var(--text-xs)',
      color: 'var(--text-dim)',
      ...style
    }
  }, rest), hints.map((h, i) => /*#__PURE__*/React.createElement(React.Fragment, {
    key: i
  }, i > 0 && /*#__PURE__*/React.createElement("span", {
    style: {
      color: 'var(--line-bright)',
      margin: '0 6px'
    }
  }, "\u2022"), /*#__PURE__*/React.createElement("span", {
    style: {
      display: 'inline-flex',
      alignItems: 'center',
      gap: 6
    }
  }, /*#__PURE__*/React.createElement("span", {
    style: {
      color: 'var(--neon-cyan)',
      fontWeight: 700
    }
  }, h.keys), /*#__PURE__*/React.createElement("span", null, h.label)))));
}
Object.assign(__ds_scope, { KeyHint });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/feedback/KeyHint.jsx", error: String((e && e.message) || e) }); }

// components/feedback/Progress.jsx
try { (() => {
function _extends() { return _extends = Object.assign ? Object.assign.bind() : function (n) { for (var e = 1; e < arguments.length; e++) { var t = arguments[e]; for (var r in t) ({}).hasOwnProperty.call(t, r) && (n[r] = t[r]); } return n; }, _extends.apply(null, arguments); }
/**
 * Progress — the Bubbles progress bar. A gradient-filled track with an
 * optional trailing percentage. `blocks` switches to a character-cell
 * rendering (█░) for full terminal authenticity.
 */
function Progress({
  value = 0,
  gradient = 'var(--grad-neon-bar)',
  showPercent = true,
  blocks = false,
  width = 280,
  style,
  ...rest
}) {
  const pct = Math.max(0, Math.min(100, value));
  if (blocks) {
    const cells = 24;
    const filled = Math.round(pct / 100 * cells);
    return /*#__PURE__*/React.createElement("div", _extends({
      style: {
        display: 'flex',
        alignItems: 'center',
        gap: 10,
        fontFamily: 'var(--font-mono)',
        fontSize: 'var(--text-sm)',
        ...style
      }
    }, rest), /*#__PURE__*/React.createElement("span", {
      style: {
        letterSpacing: '-1px',
        whiteSpace: 'pre'
      }
    }, /*#__PURE__*/React.createElement("span", {
      style: {
        background: gradient,
        WebkitBackgroundClip: 'text',
        backgroundClip: 'text',
        color: 'transparent'
      }
    }, '█'.repeat(filled)), /*#__PURE__*/React.createElement("span", {
      style: {
        color: 'var(--line)'
      }
    }, '░'.repeat(cells - filled))), showPercent && /*#__PURE__*/React.createElement("span", {
      style: {
        color: 'var(--neon-cyan)',
        fontWeight: 700
      }
    }, pct, "%"));
  }
  return /*#__PURE__*/React.createElement("div", _extends({
    style: {
      display: 'flex',
      alignItems: 'center',
      gap: 12,
      fontFamily: 'var(--font-mono)',
      ...style
    }
  }, rest), /*#__PURE__*/React.createElement("div", {
    style: {
      position: 'relative',
      width,
      height: 10,
      background: 'var(--bg-inset)',
      border: '1px solid var(--line-dim)',
      overflow: 'hidden'
    }
  }, /*#__PURE__*/React.createElement("div", {
    style: {
      position: 'absolute',
      inset: 0,
      width: `${pct}%`,
      background: gradient,
      transition: 'width var(--dur-slow) var(--ease-out)'
    }
  })), showPercent && /*#__PURE__*/React.createElement("span", {
    style: {
      color: 'var(--text-bright)',
      fontSize: 'var(--text-xs)',
      fontWeight: 700,
      minWidth: 34
    }
  }, pct, "%"));
}
Object.assign(__ds_scope, { Progress });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/feedback/Progress.jsx", error: String((e && e.message) || e) }); }

// components/feedback/Spinner.jsx
try { (() => {
function _extends() { return _extends = Object.assign ? Object.assign.bind() : function (n) { for (var e = 1; e < arguments.length; e++) { var t = arguments[e]; for (var r in t) ({}).hasOwnProperty.call(t, r) && (n[r] = t[r]); } return n; }, _extends.apply(null, arguments); }
const FRAMES = {
  braille: ['⣾', '⣽', '⣻', '⢿', '⡿', '⣟', '⣯', '⣷'],
  dots: ['⠋', '⠙', '⠹', '⠸', '⠼', '⠴', '⠦', '⠧', '⠇', '⠏'],
  line: ['|', '/', '-', '\\'],
  moon: ['🌑', '🌒', '🌓', '🌔', '🌕', '🌖', '🌗', '🌘'],
  points: ['∙∙∙', '●∙∙', '∙●∙', '∙∙●']
};

/**
 * Spinner — the Bubbles spinner. Cycles a frame set on an interval and
 * glows in the chosen tone. Pair with a label for a loading line.
 */
function Spinner({
  kind = 'braille',
  tone = 'accent',
  label,
  speed = 90,
  style,
  ...rest
}) {
  const frames = FRAMES[kind] || FRAMES.braille;
  const [i, setI] = React.useState(0);
  React.useEffect(() => {
    const reduce = window.matchMedia && window.matchMedia('(prefers-reduced-motion: reduce)').matches;
    if (reduce) return;
    const id = setInterval(() => setI(n => (n + 1) % frames.length), speed);
    return () => clearInterval(id);
  }, [frames.length, speed]);
  const tones = {
    accent: 'var(--charm-pink)',
    primary: 'var(--charm-purple)',
    info: 'var(--neon-cyan)',
    success: 'var(--neon-mint)',
    warning: 'var(--neon-gold)'
  };
  const c = tones[tone] || tones.accent;
  return /*#__PURE__*/React.createElement("span", _extends({
    style: {
      display: 'inline-flex',
      alignItems: 'center',
      gap: 10,
      fontFamily: 'var(--font-mono)',
      fontSize: 'var(--text-sm)',
      ...style
    }
  }, rest), /*#__PURE__*/React.createElement("span", {
    style: {
      color: c,
      width: '1.4em',
      textAlign: 'center',
      fontSize: 'var(--text-md)'
    }
  }, frames[i]), label && /*#__PURE__*/React.createElement("span", {
    style: {
      color: 'var(--text-body)'
    }
  }, label));
}
Object.assign(__ds_scope, { Spinner });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/feedback/Spinner.jsx", error: String((e && e.message) || e) }); }

// components/forms/Button.jsx
try { (() => {
function _extends() { return _extends = Object.assign ? Object.assign.bind() : function (n) { for (var e = 1; e < arguments.length; e++) { var t = arguments[e]; for (var r in t) ({}).hasOwnProperty.call(t, r) && (n[r] = t[r]); } return n; }, _extends.apply(null, arguments); }
/**
 * Button — a Gum-style terminal button. Renders as a padded, bracket-free
 * highlight bar. `focused` adds the neon selection glow a TUI shows on the
 * active control.
 */
function Button({
  children,
  variant = 'primary',
  // 'primary' | 'accent' | 'success' | 'ghost' | 'danger'
  size = 'md',
  // 'sm' | 'md' | 'lg'
  focused = false,
  disabled = false,
  brackets = false,
  // wrap label in [ ... ] like classic prompts
  style,
  ...rest
}) {
  const pads = {
    sm: '3px 10px',
    md: '6px 18px',
    lg: '9px 28px'
  };
  const variants = {
    primary: {
      bg: 'var(--charm-purple)',
      fg: 'var(--text-on-purple)',
      bd: 'var(--charm-purple)',
      glow: 'var(--glow-purple)'
    },
    accent: {
      bg: 'var(--charm-pink)',
      fg: 'var(--text-on-accent)',
      bd: 'var(--charm-pink)',
      glow: 'var(--glow-pink)'
    },
    success: {
      bg: 'var(--neon-mint)',
      fg: 'var(--text-on-accent)',
      bd: 'var(--neon-mint)',
      glow: 'var(--glow-mint)'
    },
    danger: {
      bg: 'var(--neon-coral)',
      fg: 'var(--text-on-accent)',
      bd: 'var(--neon-coral)',
      glow: '0 0 12px rgba(255,110,94,0.5)'
    },
    ghost: {
      bg: 'transparent',
      fg: 'var(--text-body)',
      bd: 'var(--line)',
      glow: 'none'
    }
  };
  const v = variants[variant] || variants.primary;
  return /*#__PURE__*/React.createElement("button", _extends({
    disabled: disabled,
    style: {
      display: 'inline-flex',
      alignItems: 'center',
      justifyContent: 'center',
      gap: 8,
      padding: pads[size],
      fontFamily: 'var(--font-mono)',
      fontSize: 'var(--text-sm)',
      fontWeight: 700,
      letterSpacing: '0.02em',
      lineHeight: 1,
      color: v.fg,
      background: v.bg,
      border: `1.5px solid ${focused ? 'var(--neon-cyan)' : v.bd}`,
      borderRadius: 0,
      cursor: disabled ? 'not-allowed' : 'pointer',
      opacity: disabled ? 0.4 : 1,
      ...style
    }
  }, rest), brackets ? /*#__PURE__*/React.createElement(React.Fragment, null, "[ ", children, " ]") : children);
}
Object.assign(__ds_scope, { Button });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/forms/Button.jsx", error: String((e && e.message) || e) }); }

// components/forms/Checkbox.jsx
try { (() => {
function _extends() { return _extends = Object.assign ? Object.assign.bind() : function (n) { for (var e = 1; e < arguments.length; e++) { var t = arguments[e]; for (var r in t) ({}).hasOwnProperty.call(t, r) && (n[r] = t[r]); } return n; }, _extends.apply(null, arguments); }
/**
 * Checkbox — a Huh?-style option row. `[x]` checked in mint, `[ ]` empty.
 * `cursor` draws the selection caret + highlight for the focused row.
 */
function Checkbox({
  checked = false,
  label,
  cursor = false,
  disabled = false,
  style,
  ...rest
}) {
  return /*#__PURE__*/React.createElement("div", _extends({
    style: {
      display: 'flex',
      alignItems: 'center',
      gap: 8,
      padding: '4px 10px',
      borderRadius: 'var(--radius-sm)',
      fontFamily: 'var(--font-mono)',
      fontSize: 'var(--text-sm)',
      color: disabled ? 'var(--text-dim)' : cursor ? 'var(--text-bright)' : 'var(--text-body)',
      background: cursor ? 'var(--tint-primary)' : 'transparent',
      boxShadow: cursor ? 'inset 2px 0 0 var(--charm-pink)' : 'none',
      opacity: disabled ? 0.5 : 1,
      cursor: disabled ? 'not-allowed' : 'pointer',
      ...style
    }
  }, rest), /*#__PURE__*/React.createElement("span", {
    style: {
      color: 'var(--charm-pink)',
      width: '1ch',
      fontWeight: 700
    }
  }, cursor ? '›' : ' '), /*#__PURE__*/React.createElement("span", {
    style: {
      color: checked ? 'var(--neon-mint)' : 'var(--text-dim)',
      fontWeight: 700,
      whiteSpace: 'pre'
    }
  }, checked ? '[✓]' : '[ ]'), /*#__PURE__*/React.createElement("span", null, label));
}
Object.assign(__ds_scope, { Checkbox });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/forms/Checkbox.jsx", error: String((e && e.message) || e) }); }

// components/forms/TextInput.jsx
try { (() => {
function _extends() { return _extends = Object.assign ? Object.assign.bind() : function (n) { for (var e = 1; e < arguments.length; e++) { var t = arguments[e]; for (var r in t) ({}).hasOwnProperty.call(t, r) && (n[r] = t[r]); } return n; }, _extends.apply(null, arguments); }
/**
 * TextInput — the Bubbles textinput. A prompt glyph, the entered value,
 * a blinking block cursor, and dim placeholder text. `focused` lights the
 * border cyan; leave it off for an idle field.
 */
function TextInput({
  value = '',
  placeholder = '',
  prompt = '> ',
  focused = true,
  bordered = true,
  label,
  width = 320,
  style,
  ...rest
}) {
  const showPlaceholder = !value;
  return /*#__PURE__*/React.createElement("div", _extends({
    style: {
      display: 'flex',
      flexDirection: 'column',
      gap: 6,
      width,
      fontFamily: 'var(--font-mono)',
      ...style
    }
  }, rest), label && /*#__PURE__*/React.createElement("span", {
    style: {
      fontSize: 'var(--text-xs)',
      color: 'var(--text-muted)',
      letterSpacing: '0.04em'
    }
  }, label), /*#__PURE__*/React.createElement("div", {
    style: {
      display: 'flex',
      alignItems: 'center',
      padding: bordered ? '8px 12px' : '2px 0',
      background: bordered ? 'var(--bg-inset)' : 'transparent',
      border: bordered ? `1px solid ${focused ? 'var(--neon-cyan)' : 'var(--line)'}` : 'none',
      borderRadius: 'var(--radius-sm)',
      fontSize: 'var(--text-sm)'
    }
  }, /*#__PURE__*/React.createElement("span", {
    style: {
      color: 'var(--charm-pink)',
      fontWeight: 700,
      whiteSpace: 'pre'
    }
  }, prompt), /*#__PURE__*/React.createElement("span", {
    style: {
      color: showPlaceholder ? 'var(--text-dim)' : 'var(--text-bright)',
      whiteSpace: 'pre'
    }
  }, showPlaceholder ? placeholder : value), focused && /*#__PURE__*/React.createElement("span", {
    style: {
      display: 'inline-block',
      width: '0.55em',
      height: '1.15em',
      marginLeft: 1,
      background: 'var(--cursor)',
      animation: 'tui-blink var(--blink) steps(1) infinite',
      verticalAlign: 'text-bottom'
    }
  })));
}
Object.assign(__ds_scope, { TextInput });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/forms/TextInput.jsx", error: String((e && e.message) || e) }); }

// components/forms/Toggle.jsx
try { (() => {
function _extends() { return _extends = Object.assign ? Object.assign.bind() : function (n) { for (var e = 1; e < arguments.length; e++) { var t = arguments[e]; for (var r in t) ({}).hasOwnProperty.call(t, r) && (n[r] = t[r]); } return n; }, _extends.apply(null, arguments); }
/**
 * Toggle — a segmented on/off switch styled like a Gum choose. The active
 * side is a glowing pill; the inactive side stays dim.
 */
function Toggle({
  on = false,
  onLabel = 'on',
  offLabel = 'off',
  tone = 'success',
  style,
  ...rest
}) {
  const tones = {
    success: 'var(--neon-mint)',
    primary: 'var(--charm-purple)',
    accent: 'var(--charm-pink)',
    info: 'var(--neon-cyan)'
  };
  const active = tones[tone] || tones.success;
  const seg = (label, isActive) => ({
    padding: '4px 14px',
    fontFamily: 'var(--font-mono)',
    fontSize: 'var(--text-xs)',
    fontWeight: 700,
    letterSpacing: '0.06em',
    textTransform: 'uppercase',
    color: isActive ? 'var(--text-on-accent)' : 'var(--text-dim)',
    background: isActive ? active : 'transparent',
    transition: 'color var(--dur-base) var(--ease-out), background-color var(--dur-base) var(--ease-out), box-shadow var(--dur-base) var(--ease-out)'
  });
  return /*#__PURE__*/React.createElement("div", _extends({
    style: {
      display: 'inline-flex',
      alignItems: 'stretch',
      border: '1.5px solid var(--line)',
      borderRadius: 0,
      overflow: 'hidden',
      background: 'var(--bg-inset)',
      ...style
    },
    role: "switch",
    "aria-checked": on
  }, rest), /*#__PURE__*/React.createElement("span", {
    style: seg(offLabel, !on)
  }, offLabel), /*#__PURE__*/React.createElement("span", {
    style: seg(onLabel, on)
  }, onLabel));
}
Object.assign(__ds_scope, { Toggle });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/forms/Toggle.jsx", error: String((e && e.message) || e) }); }

// components/terminal/Kbd.jsx
try { (() => {
function _extends() { return _extends = Object.assign ? Object.assign.bind() : function (n) { for (var e = 1; e < arguments.length; e++) { var t = arguments[e]; for (var r in t) ({}).hasOwnProperty.call(t, r) && (n[r] = t[r]); } return n; }, _extends.apply(null, arguments); }
/**
 * Kbd — a single keycap glyph, as used in help lines (↑ ↓ enter q).
 * Rendered as a soft-square terminal key.
 */
function Kbd({
  children,
  tone = 'default',
  style,
  ...rest
}) {
  const tones = {
    default: {
      border: 'var(--line)',
      color: 'var(--text-body)',
      bg: 'var(--bg-surface-2)'
    },
    accent: {
      border: 'var(--charm-pink)',
      color: 'var(--charm-pink-hi)',
      bg: 'var(--tint-accent)'
    },
    info: {
      border: 'var(--neon-cyan)',
      color: 'var(--neon-cyan-hi)',
      bg: 'var(--tint-info)'
    }
  }[tone];
  return /*#__PURE__*/React.createElement("kbd", _extends({
    style: {
      display: 'inline-flex',
      alignItems: 'center',
      justifyContent: 'center',
      minWidth: 22,
      height: 22,
      padding: '0 6px',
      fontFamily: 'var(--font-mono)',
      fontSize: 'var(--text-xs)',
      fontWeight: 700,
      lineHeight: 1,
      border: `1px solid ${tones.border}`,
      borderRadius: 0,
      background: tones.bg,
      color: tones.color,
      ...style
    }
  }, rest), children);
}
Object.assign(__ds_scope, { Kbd });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/terminal/Kbd.jsx", error: String((e && e.message) || e) }); }

// components/terminal/StatusBar.jsx
try { (() => {
function _extends() { return _extends = Object.assign ? Object.assign.bind() : function (n) { for (var e = 1; e < arguments.length; e++) { var t = arguments[e]; for (var r in t) ({}).hasOwnProperty.call(t, r) && (n[r] = t[r]); } return n; }, _extends.apply(null, arguments); }
/**
 * StatusBar — a Lip Gloss layout-example status line. A row of colored
 * segments; each segment can be a solid "key" pill or plain text.
 */
function StatusBar({
  segments = [],
  style,
  ...rest
}) {
  const tones = {
    primary: ['var(--charm-purple)', 'var(--text-on-purple)'],
    accent: ['var(--charm-pink)', 'var(--text-on-accent)'],
    info: ['var(--neon-cyan)', 'var(--text-on-accent)'],
    success: ['var(--neon-mint)', 'var(--text-on-accent)'],
    warning: ['var(--neon-gold)', 'var(--text-on-accent)'],
    danger: ['var(--neon-coral)', 'var(--text-on-accent)'],
    muted: ['var(--bg-surface-2)', 'var(--text-muted)']
  };
  return /*#__PURE__*/React.createElement("div", _extends({
    style: {
      display: 'flex',
      alignItems: 'stretch',
      fontFamily: 'var(--font-mono)',
      fontSize: 'var(--text-xs)',
      borderTop: '1px solid var(--line-dim)',
      background: 'var(--bg-inset)',
      ...style
    }
  }, rest), segments.map((seg, i) => {
    const solid = seg.tone && seg.tone !== 'plain';
    const [bg, fg] = tones[seg.tone] || tones.muted;
    return /*#__PURE__*/React.createElement("span", {
      key: i,
      style: {
        display: 'inline-flex',
        alignItems: 'center',
        gap: 6,
        padding: '5px 12px',
        flex: seg.grow ? 1 : '0 0 auto',
        justifyContent: seg.grow ? 'flex-start' : 'center',
        background: solid ? bg : 'transparent',
        color: solid ? fg : 'var(--text-muted)',
        fontWeight: solid ? 700 : 400,
        letterSpacing: solid ? '0.04em' : 0
      }
    }, seg.icon && /*#__PURE__*/React.createElement("span", {
      "aria-hidden": "true"
    }, seg.icon), seg.label);
  }));
}
Object.assign(__ds_scope, { StatusBar });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/terminal/StatusBar.jsx", error: String((e && e.message) || e) }); }

// components/terminal/TerminalWindow.jsx
try { (() => {
function _extends() { return _extends = Object.assign ? Object.assign.bind() : function (n) { for (var e = 1; e < arguments.length; e++) { var t = arguments[e]; for (var r in t) ({}).hasOwnProperty.call(t, r) && (n[r] = t[r]); } return n; }, _extends.apply(null, arguments); }
/**
 * TerminalWindow — the framing chrome every Charm TUI lives inside.
 * Rounded Lip Gloss-style border, neon title dots, optional glow, and
 * an optional status bar slot at the bottom.
 */
function TerminalWindow({
  title = 'bubbletea',
  dots = true,
  glow = 'purple',
  // 'purple' | 'pink' | 'cyan' | 'none'
  padding = 'var(--space-6)',
  statusBar = null,
  width,
  style,
  children,
  ...rest
}) {
  const glowShadow = {
    purple: 'var(--shadow-window), var(--glow-soft)',
    pink: 'var(--shadow-window), 0 0 32px rgba(255,95,162,0.22)',
    cyan: 'var(--shadow-window), 0 0 32px rgba(78,230,255,0.22)',
    none: 'var(--shadow-window)'
  }[glow] || 'var(--shadow-window)';
  const dotColors = ['var(--charm-pink)', 'var(--neon-gold)', 'var(--neon-mint)'];
  return /*#__PURE__*/React.createElement("div", _extends({
    style: {
      width,
      display: 'flex',
      flexDirection: 'column',
      background: 'var(--bg-terminal)',
      border: '1.5px solid var(--line)',
      borderRadius: 'var(--radius-lg)',
      boxShadow: glowShadow,
      fontFamily: 'var(--font-mono)',
      color: 'var(--text-body)',
      overflow: 'hidden',
      ...style
    }
  }, rest), /*#__PURE__*/React.createElement("div", {
    style: {
      display: 'flex',
      alignItems: 'center',
      gap: 'var(--space-4)',
      padding: '10px var(--space-5)',
      borderBottom: '1px solid var(--line-dim)',
      background: 'linear-gradient(180deg, var(--tint-primary), transparent)',
      flexShrink: 0
    }
  }, dots && /*#__PURE__*/React.createElement("div", {
    style: {
      display: 'flex',
      gap: '7px'
    }
  }, dotColors.map((c, i) => /*#__PURE__*/React.createElement("span", {
    key: i,
    style: {
      width: 11,
      height: 11,
      borderRadius: '50%',
      background: c,
      boxShadow: `0 0 8px ${c}`
    }
  }))), /*#__PURE__*/React.createElement("span", {
    style: {
      flex: 1,
      textAlign: dots ? 'center' : 'left',
      fontSize: 'var(--text-xs)',
      letterSpacing: '0.06em',
      color: 'var(--text-muted)',
      fontWeight: 500,
      marginRight: dots ? 46 : 0,
      whiteSpace: 'nowrap',
      overflow: 'hidden',
      textOverflow: 'ellipsis'
    }
  }, title)), /*#__PURE__*/React.createElement("div", {
    style: {
      padding,
      flex: 1,
      minHeight: 0
    }
  }, children), statusBar && /*#__PURE__*/React.createElement("div", {
    style: {
      flexShrink: 0
    }
  }, statusBar));
}
Object.assign(__ds_scope, { TerminalWindow });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/terminal/TerminalWindow.jsx", error: String((e && e.message) || e) }); }

// ui_kits/charm-cli/App.export.jsx
try { (() => {
// charm-cli — single-file export bundle. Boot-gated so nothing touches the
// design-system namespace until the inlined bundle has defined it (race-proof).
(function boot() {
  const NS = window.BubbleteaTUIDesignSystem_5c2f37;
  if (!NS || !NS.TerminalWindow) {
    return void requestAnimationFrame(boot);
  }
  if (window.__charmCliMounted) return;
  window.__charmCliMounted = true;
  const {
    TerminalWindow,
    StatusBar,
    List,
    KeyHint,
    Button,
    Badge,
    TextInput,
    Checkbox,
    Toggle,
    Tabs,
    Spinner,
    Progress,
    Dialog
  } = NS;
  const TEMPLATES = [{
    title: 'Bubble Tea App',
    desc: 'A full TUI, Elm-architecture starter',
    badge: '★ popular'
  }, {
    title: 'Gum Script',
    desc: 'Glue shell scripts with interactive prompts'
  }, {
    title: 'Glow Doc Site',
    desc: 'A markdown reader for your docs'
  }, {
    title: 'Wish SSH App',
    desc: 'Serve your TUI over SSH'
  }, {
    title: 'Soft Serve',
    desc: 'A self-hostable git server'
  }];
  const TEMPLATE_NAMES = TEMPLATES.map(t => t.title);
  function MenuScreen({
    selected,
    onSelect,
    onContinue
  }) {
    return /*#__PURE__*/React.createElement("div", {
      style: {
        display: 'flex',
        flexDirection: 'column',
        gap: 16
      }
    }, /*#__PURE__*/React.createElement("div", {
      style: {
        display: 'flex',
        alignItems: 'center',
        gap: 14
      }
    }, /*#__PURE__*/React.createElement("span", {
      style: {
        fontSize: 'var(--text-lg)',
        lineHeight: 1
      },
      "aria-hidden": "true"
    }, "\uD83E\uDDCB"), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("div", {
      style: {
        fontSize: 'var(--text-base)',
        fontWeight: 700,
        color: 'var(--text-bright)'
      }
    }, "What would you like to brew?"), /*#__PURE__*/React.createElement("div", {
      style: {
        fontSize: 'var(--text-xs)',
        color: 'var(--text-muted)',
        marginTop: 2
      }
    }, "Scaffold a new project from a Charm template.")), /*#__PURE__*/React.createElement("span", {
      style: {
        marginLeft: 'auto'
      }
    }, /*#__PURE__*/React.createElement(Badge, {
      tone: "info",
      variant: "outline"
    }, "charm v2.0.6"))), /*#__PURE__*/React.createElement("div", {
      style: {
        background: 'var(--bg-surface)',
        border: '1px solid var(--line-dim)',
        borderRadius: 'var(--radius-md)',
        padding: '8px 4px'
      }
    }, TEMPLATES.map((t, i) => /*#__PURE__*/React.createElement("div", {
      key: i,
      onClick: () => onSelect(i),
      onDoubleClick: onContinue,
      style: {
        cursor: 'pointer'
      }
    }, /*#__PURE__*/React.createElement(List, {
      selected: i === selected ? 0 : -1,
      items: [t],
      style: {
        pointerEvents: 'none'
      }
    })))), /*#__PURE__*/React.createElement("div", {
      style: {
        display: 'flex',
        alignItems: 'center',
        gap: 14
      }
    }, /*#__PURE__*/React.createElement(KeyHint, {
      hints: [{
        keys: '↑/↓',
        label: 'navigate'
      }, {
        keys: 'enter',
        label: 'select'
      }, {
        keys: 'q',
        label: 'quit'
      }]
    }), /*#__PURE__*/React.createElement("span", {
      style: {
        marginLeft: 'auto'
      }
    }, /*#__PURE__*/React.createElement(Button, {
      variant: "accent",
      focused: true,
      onClick: onContinue
    }, "Continue \u2192"))));
  }
  function FormScreen({
    config,
    setConfig,
    onBack,
    onCreate
  }) {
    const set = patch => setConfig({
      ...config,
      ...patch
    });
    const flag = k => set({
      [k]: !config[k]
    });
    return /*#__PURE__*/React.createElement("div", {
      style: {
        display: 'flex',
        flexDirection: 'column',
        gap: 16
      }
    }, /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("div", {
      style: {
        fontSize: 'var(--text-base)',
        fontWeight: 700,
        color: 'var(--text-bright)'
      }
    }, "Configure ", /*#__PURE__*/React.createElement("span", {
      style: {
        color: 'var(--charm-pink)'
      }
    }, config.template)), /*#__PURE__*/React.createElement("div", {
      style: {
        fontSize: 'var(--text-xs)',
        color: 'var(--text-muted)',
        marginTop: 2
      }
    }, "These map to your Bubble Tea program options.")), /*#__PURE__*/React.createElement(Tabs, {
      active: 0,
      tabs: ['General', 'Options', 'Theme']
    }), /*#__PURE__*/React.createElement("div", {
      style: {
        display: 'grid',
        gridTemplateColumns: '1fr 1fr',
        gap: 20,
        alignItems: 'start',
        background: 'var(--bg-surface)',
        border: '1px solid var(--line-dim)',
        borderRadius: 'var(--radius-md)',
        padding: 18
      }
    }, /*#__PURE__*/React.createElement("div", {
      style: {
        display: 'flex',
        flexDirection: 'column',
        gap: 14
      }
    }, /*#__PURE__*/React.createElement(TextInput, {
      label: "Project name",
      value: config.name,
      focused: config.focus === 'name',
      width: "100%"
    }), /*#__PURE__*/React.createElement(TextInput, {
      label: "Module path",
      value: config.module,
      focused: config.focus === 'module',
      width: "100%"
    }), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("div", {
      style: {
        fontSize: 'var(--text-xs)',
        color: 'var(--text-muted)',
        marginBottom: 6,
        letterSpacing: '0.04em'
      }
    }, "Color profile"), /*#__PURE__*/React.createElement("div", {
      onClick: () => flag('trueColor'),
      style: {
        cursor: 'pointer',
        display: 'inline-block'
      }
    }, /*#__PURE__*/React.createElement(Toggle, {
      on: config.trueColor,
      onLabel: "truecolor",
      offLabel: "256",
      tone: "info"
    })))), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("div", {
      style: {
        fontSize: 'var(--text-xs)',
        color: 'var(--text-muted)',
        marginBottom: 6,
        letterSpacing: '0.04em'
      }
    }, "Program options"), /*#__PURE__*/React.createElement("div", {
      onClick: () => flag('mouse'),
      style: {
        cursor: 'pointer'
      }
    }, /*#__PURE__*/React.createElement(Checkbox, {
      checked: config.mouse,
      label: "WithMouseCellMotion()",
      cursor: config.focus === 'opts'
    })), /*#__PURE__*/React.createElement("div", {
      onClick: () => flag('alt'),
      style: {
        cursor: 'pointer'
      }
    }, /*#__PURE__*/React.createElement(Checkbox, {
      checked: config.alt,
      label: "WithAltScreen()"
    })), /*#__PURE__*/React.createElement("div", {
      onClick: () => flag('logging'),
      style: {
        cursor: 'pointer'
      }
    }, /*#__PURE__*/React.createElement(Checkbox, {
      checked: config.logging,
      label: "LogToFile()"
    })), /*#__PURE__*/React.createElement("div", {
      onClick: () => flag('report'),
      style: {
        cursor: 'pointer'
      }
    }, /*#__PURE__*/React.createElement(Checkbox, {
      checked: config.report,
      label: "WithReportFocus()"
    })))), /*#__PURE__*/React.createElement("div", {
      style: {
        display: 'flex',
        alignItems: 'center',
        gap: 14
      }
    }, /*#__PURE__*/React.createElement(KeyHint, {
      hints: [{
        keys: 'tab',
        label: 'next field'
      }, {
        keys: 'space',
        label: 'toggle'
      }, {
        keys: 'esc',
        label: 'back'
      }]
    }), /*#__PURE__*/React.createElement("span", {
      style: {
        marginLeft: 'auto',
        display: 'flex',
        gap: 10
      }
    }, /*#__PURE__*/React.createElement(Button, {
      variant: "ghost",
      onClick: onBack
    }, "\u2190 Back"), /*#__PURE__*/React.createElement(Button, {
      variant: "success",
      focused: true,
      onClick: onCreate
    }, "Create app \u2713"))));
  }
  const STEPS = ['Resolving charm.land/bubbletea/v2', 'Downloading lipgloss, bubbles, harmonica', 'Writing main.go, model.go, view.go', 'go mod tidy', 'Rendering rounded borders', 'Steeping the boba…'];
  function InstallScreen({
    config,
    onRestart
  }) {
    const [pct, setPct] = React.useState(0);
    const [line, setLine] = React.useState(0);
    const done = pct >= 100;
    React.useEffect(() => {
      const reduce = window.matchMedia && window.matchMedia('(prefers-reduced-motion: reduce)').matches;
      if (reduce) {
        setPct(100);
        setLine(STEPS.length);
        return;
      }
      const id = setInterval(() => {
        setPct(p => {
          const next = Math.min(100, p + 4);
          setLine(Math.min(STEPS.length, Math.floor(next / 100 * STEPS.length)));
          if (next >= 100) clearInterval(id);
          return next;
        });
      }, 130);
      return () => clearInterval(id);
    }, []);
    return /*#__PURE__*/React.createElement("div", {
      style: {
        display: 'flex',
        flexDirection: 'column',
        gap: 16,
        minHeight: 300
      }
    }, /*#__PURE__*/React.createElement("div", {
      style: {
        display: 'flex',
        alignItems: 'center',
        gap: 12
      }
    }, done ? /*#__PURE__*/React.createElement("span", {
      style: {
        color: 'var(--neon-mint)',
        fontSize: 'var(--text-md)'
      }
    }, "\u2713") : /*#__PURE__*/React.createElement(Spinner, {
      kind: "braille",
      tone: "accent"
    }), /*#__PURE__*/React.createElement("div", {
      style: {
        fontSize: 'var(--text-base)',
        fontWeight: 700,
        color: 'var(--text-bright)'
      }
    }, done ? 'Your app is ready' : 'Brewing your app…'), /*#__PURE__*/React.createElement("span", {
      style: {
        marginLeft: 'auto'
      }
    }, /*#__PURE__*/React.createElement(Badge, {
      tone: done ? 'success' : 'primary',
      dot: true
    }, done ? 'done' : 'installing'))), /*#__PURE__*/React.createElement(Progress, {
      value: pct,
      gradient: "var(--grad-neon-bar)",
      width: "100%"
    }), /*#__PURE__*/React.createElement("div", {
      style: {
        flex: 1,
        background: 'var(--bg-inset)',
        border: '1px solid var(--line-dim)',
        borderRadius: 'var(--radius-md)',
        padding: 14,
        fontFamily: 'var(--font-mono)',
        fontSize: 'var(--text-xs)',
        lineHeight: 1.9,
        overflow: 'hidden'
      }
    }, STEPS.slice(0, line).map((s, i) => /*#__PURE__*/React.createElement("div", {
      key: i,
      style: {
        color: 'var(--text-muted)'
      }
    }, /*#__PURE__*/React.createElement("span", {
      style: {
        color: 'var(--neon-mint)'
      }
    }, "\u2713"), " ", s)), !done && line < STEPS.length && /*#__PURE__*/React.createElement("div", {
      style: {
        color: 'var(--text-bright)'
      }
    }, /*#__PURE__*/React.createElement("span", {
      style: {
        color: 'var(--charm-pink)'
      }
    }, "\u2192"), " ", STEPS[line], /*#__PURE__*/React.createElement("span", {
      style: {
        color: 'var(--neon-cyan)'
      }
    }, "\u258D"))), done ? /*#__PURE__*/React.createElement(Dialog, {
      title: "Next steps",
      tone: "success",
      icon: "\u2713",
      actions: /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement(Button, {
        variant: "ghost",
        onClick: onRestart
      }, "Start over"), /*#__PURE__*/React.createElement(Button, {
        variant: "success",
        focused: true,
        onClick: onRestart
      }, "Run it \u2192"))
    }, /*#__PURE__*/React.createElement("div", {
      style: {
        fontFamily: 'var(--font-mono)'
      }
    }, /*#__PURE__*/React.createElement("span", {
      style: {
        color: 'var(--text-dim)'
      }
    }, "$ "), "cd ", config.name, /*#__PURE__*/React.createElement("br", null), /*#__PURE__*/React.createElement("span", {
      style: {
        color: 'var(--text-dim)'
      }
    }, "$ "), "go run .")) : /*#__PURE__*/React.createElement(KeyHint, {
      hints: [{
        keys: 'ctrl+c',
        label: 'cancel'
      }]
    }));
  }
  function App() {
    const [screen, setScreen] = React.useState('menu');
    const [selected, setSelected] = React.useState(0);
    const [config, setConfig] = React.useState({
      template: 'Bubble Tea App',
      name: 'boba-app',
      module: 'github.com/you/boba-app',
      mouse: true,
      alt: true,
      logging: false,
      report: false,
      trueColor: true,
      focus: 'name'
    });
    const goForm = () => {
      setConfig(c => ({
        ...c,
        template: TEMPLATE_NAMES[selected]
      }));
      setScreen('form');
    };
    const step = {
      menu: 1,
      form: 2,
      install: 3
    }[screen];
    const status = [{
      label: screen === 'install' ? 'BREW' : 'NEW',
      tone: screen === 'install' ? 'success' : 'primary'
    }, {
      label: `~/${config.name}`,
      tone: 'plain',
      grow: true
    }, {
      label: `step ${step}/3`,
      tone: 'muted'
    }, {
      label: config.trueColor ? 'truecolor' : '256',
      tone: 'accent'
    }];
    return /*#__PURE__*/React.createElement("div", {
      style: {
        width: 'min(900px, 94vw)'
      }
    }, /*#__PURE__*/React.createElement(TerminalWindow, {
      title: "charm \u2014 create \u2733",
      glow: "purple",
      statusBar: /*#__PURE__*/React.createElement(StatusBar, {
        segments: status
      })
    }, screen === 'menu' && /*#__PURE__*/React.createElement(MenuScreen, {
      selected: selected,
      onSelect: setSelected,
      onContinue: goForm
    }), screen === 'form' && /*#__PURE__*/React.createElement(FormScreen, {
      config: config,
      setConfig: setConfig,
      onBack: () => setScreen('menu'),
      onCreate: () => setScreen('install')
    }), screen === 'install' && /*#__PURE__*/React.createElement(InstallScreen, {
      config: config,
      onRestart: () => setScreen('menu')
    })));
  }
  ReactDOM.createRoot(document.getElementById('root')).render(/*#__PURE__*/React.createElement(App, null));
})();
})(); } catch (e) { __ds_ns.__errors.push({ path: "ui_kits/charm-cli/App.export.jsx", error: String((e && e.message) || e) }); }

// ui_kits/charm-cli/App.jsx
try { (() => {
// App — orchestrates the charm-cli flow inside one TerminalWindow.
const {
  TerminalWindow,
  StatusBar
} = window.BubbleteaTUIDesignSystem_5c2f37;
const TEMPLATE_NAMES = ['Bubble Tea App', 'Gum Script', 'Glow Doc Site', 'Wish SSH App', 'Soft Serve'];
function ThemeToggle() {
  const [theme, setTheme] = React.useState(document.documentElement.dataset.theme === 'day' ? 'day' : 'night');
  const flip = () => {
    const t = theme === 'day' ? 'night' : 'day';
    document.documentElement.dataset.theme = t;
    localStorage.setItem('btds-theme', t);
    setTheme(t);
  };
  return /*#__PURE__*/React.createElement("button", {
    onClick: flip,
    style: {
      display: 'inline-flex',
      alignItems: 'center',
      gap: 8,
      padding: '7px 16px',
      fontFamily: 'var(--font-mono)',
      fontSize: 'var(--text-xs)',
      fontWeight: 700,
      color: 'var(--text-body)',
      background: 'var(--bg-surface)',
      border: '1.5px solid var(--line)',
      borderRadius: 'var(--radius-pill)',
      cursor: 'pointer'
    }
  }, theme === 'day' ? '☾ night' : '☀ day');
}
function App() {
  const [screen, setScreen] = React.useState('menu');
  const [selected, setSelected] = React.useState(0);
  const [config, setConfig] = React.useState({
    template: 'Bubble Tea App',
    name: 'boba-app',
    module: 'github.com/you/boba-app',
    mouse: true,
    alt: true,
    logging: false,
    report: false,
    trueColor: true,
    focus: 'name'
  });
  const goForm = () => {
    setConfig(c => ({
      ...c,
      template: TEMPLATE_NAMES[selected]
    }));
    setScreen('form');
  };
  const step = {
    menu: 1,
    form: 2,
    install: 3
  }[screen];
  const status = [{
    label: screen === 'install' ? 'BREW' : 'NEW',
    tone: screen === 'install' ? 'success' : 'primary'
  }, {
    label: `~/${config.name}`,
    tone: 'plain',
    grow: true
  }, {
    label: `step ${step}/3`,
    tone: 'muted'
  }, {
    label: config.trueColor ? 'truecolor' : '256',
    tone: 'accent'
  }];
  return /*#__PURE__*/React.createElement("div", {
    style: {
      width: 'min(900px, 94vw)'
    }
  }, /*#__PURE__*/React.createElement("div", {
    style: {
      display: 'flex',
      justifyContent: 'flex-end',
      marginBottom: 14
    }
  }, /*#__PURE__*/React.createElement(ThemeToggle, null)), /*#__PURE__*/React.createElement(TerminalWindow, {
    title: "charm \u2014 create \u2733",
    glow: "purple",
    statusBar: /*#__PURE__*/React.createElement(StatusBar, {
      segments: status
    })
  }, screen === 'menu' && /*#__PURE__*/React.createElement(window.MenuScreen, {
    selected: selected,
    onSelect: setSelected,
    onContinue: goForm
  }), screen === 'form' && /*#__PURE__*/React.createElement(window.FormScreen, {
    config: config,
    setConfig: setConfig,
    onBack: () => setScreen('menu'),
    onCreate: () => setScreen('install')
  }), screen === 'install' && /*#__PURE__*/React.createElement(window.InstallScreen, {
    config: config,
    onRestart: () => {
      setScreen('menu');
    }
  })));
}
ReactDOM.createRoot(document.getElementById('root')).render(/*#__PURE__*/React.createElement(App, null));
})(); } catch (e) { __ds_ns.__errors.push({ path: "ui_kits/charm-cli/App.jsx", error: String((e && e.message) || e) }); }

// ui_kits/charm-cli/FormScreen.jsx
try { (() => {
// FormScreen — configure the new app. Huh?-style inputs, checkboxes, toggle.
const {
  TextInput,
  Checkbox,
  Toggle,
  Button,
  KeyHint,
  Tabs
} = window.BubbleteaTUIDesignSystem_5c2f37;
function FormScreen({
  config,
  setConfig,
  onBack,
  onCreate
}) {
  const set = patch => setConfig({
    ...config,
    ...patch
  });
  const flag = k => set({
    [k]: !config[k]
  });
  return /*#__PURE__*/React.createElement("div", {
    style: {
      display: 'flex',
      flexDirection: 'column',
      gap: 16
    }
  }, /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("div", {
    style: {
      fontSize: 'var(--text-base)',
      fontWeight: 700,
      color: 'var(--text-bright)'
    }
  }, "Configure ", /*#__PURE__*/React.createElement("span", {
    style: {
      color: 'var(--charm-pink)'
    }
  }, config.template)), /*#__PURE__*/React.createElement("div", {
    style: {
      fontSize: 'var(--text-xs)',
      color: 'var(--text-muted)',
      marginTop: 2
    }
  }, "These map to your Bubble Tea program options.")), /*#__PURE__*/React.createElement(Tabs, {
    active: 0,
    tabs: ['General', 'Options', 'Theme']
  }), /*#__PURE__*/React.createElement("div", {
    style: {
      display: 'grid',
      gridTemplateColumns: '1fr 1fr',
      gap: 20,
      alignItems: 'start',
      background: 'var(--bg-surface)',
      border: '1px solid var(--line-dim)',
      borderRadius: 'var(--radius-md)',
      padding: 18
    }
  }, /*#__PURE__*/React.createElement("div", {
    style: {
      display: 'flex',
      flexDirection: 'column',
      gap: 14
    }
  }, /*#__PURE__*/React.createElement(TextInput, {
    label: "Project name",
    value: config.name,
    focused: config.focus === 'name',
    width: "100%"
  }), /*#__PURE__*/React.createElement(TextInput, {
    label: "Module path",
    value: config.module,
    focused: config.focus === 'module',
    width: "100%"
  }), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("div", {
    style: {
      fontSize: 'var(--text-xs)',
      color: 'var(--text-muted)',
      marginBottom: 6,
      letterSpacing: '0.04em'
    }
  }, "Color profile"), /*#__PURE__*/React.createElement("div", {
    onClick: () => flag('trueColor'),
    style: {
      cursor: 'pointer',
      display: 'inline-block'
    }
  }, /*#__PURE__*/React.createElement(Toggle, {
    on: config.trueColor,
    onLabel: "truecolor",
    offLabel: "256",
    tone: "info"
  })))), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("div", {
    style: {
      fontSize: 'var(--text-xs)',
      color: 'var(--text-muted)',
      marginBottom: 6,
      letterSpacing: '0.04em'
    }
  }, "Program options"), /*#__PURE__*/React.createElement("div", {
    onClick: () => flag('mouse'),
    style: {
      cursor: 'pointer'
    }
  }, /*#__PURE__*/React.createElement(Checkbox, {
    checked: config.mouse,
    label: "WithMouseCellMotion()",
    cursor: config.focus === 'opts'
  })), /*#__PURE__*/React.createElement("div", {
    onClick: () => flag('alt'),
    style: {
      cursor: 'pointer'
    }
  }, /*#__PURE__*/React.createElement(Checkbox, {
    checked: config.alt,
    label: "WithAltScreen()"
  })), /*#__PURE__*/React.createElement("div", {
    onClick: () => flag('logging'),
    style: {
      cursor: 'pointer'
    }
  }, /*#__PURE__*/React.createElement(Checkbox, {
    checked: config.logging,
    label: "LogToFile()"
  })), /*#__PURE__*/React.createElement("div", {
    onClick: () => flag('report'),
    style: {
      cursor: 'pointer'
    }
  }, /*#__PURE__*/React.createElement(Checkbox, {
    checked: config.report,
    label: "WithReportFocus()"
  })))), /*#__PURE__*/React.createElement("div", {
    style: {
      display: 'flex',
      alignItems: 'center',
      gap: 14
    }
  }, /*#__PURE__*/React.createElement(KeyHint, {
    hints: [{
      keys: 'tab',
      label: 'next field'
    }, {
      keys: 'space',
      label: 'toggle'
    }, {
      keys: 'esc',
      label: 'back'
    }]
  }), /*#__PURE__*/React.createElement("span", {
    style: {
      marginLeft: 'auto',
      display: 'flex',
      gap: 10
    }
  }, /*#__PURE__*/React.createElement(Button, {
    variant: "ghost",
    onClick: onBack
  }, "\u2190 Back"), /*#__PURE__*/React.createElement(Button, {
    variant: "success",
    focused: true,
    onClick: onCreate
  }, "Create app \u2713"))));
}
window.FormScreen = FormScreen;
})(); } catch (e) { __ds_ns.__errors.push({ path: "ui_kits/charm-cli/FormScreen.jsx", error: String((e && e.message) || e) }); }

// ui_kits/charm-cli/InstallScreen.jsx
try { (() => {
// InstallScreen — brews the app: animated progress + streaming log, then success.
const {
  Spinner,
  Progress,
  Button,
  Dialog,
  KeyHint,
  Badge
} = window.BubbleteaTUIDesignSystem_5c2f37;
const STEPS = ['Resolving charm.land/bubbletea/v2', 'Downloading lipgloss, bubbles, harmonica', 'Writing main.go, model.go, view.go', 'go mod tidy', 'Rendering rounded borders', 'Steeping the boba…'];
function InstallScreen({
  config,
  onRestart
}) {
  const [pct, setPct] = React.useState(0);
  const [line, setLine] = React.useState(0);
  const done = pct >= 100;
  React.useEffect(() => {
    const reduce = window.matchMedia && window.matchMedia('(prefers-reduced-motion: reduce)').matches;
    if (reduce) {
      setPct(100);
      setLine(STEPS.length);
      return;
    }
    const id = setInterval(() => {
      setPct(p => {
        const next = Math.min(100, p + 4);
        setLine(Math.min(STEPS.length, Math.floor(next / 100 * STEPS.length)));
        if (next >= 100) clearInterval(id);
        return next;
      });
    }, 130);
    return () => clearInterval(id);
  }, []);
  return /*#__PURE__*/React.createElement("div", {
    style: {
      display: 'flex',
      flexDirection: 'column',
      gap: 16,
      minHeight: 300
    }
  }, /*#__PURE__*/React.createElement("div", {
    style: {
      display: 'flex',
      alignItems: 'center',
      gap: 12
    }
  }, done ? /*#__PURE__*/React.createElement("span", {
    style: {
      color: 'var(--neon-mint)',
      fontSize: 'var(--text-md)'
    }
  }, "\u2713") : /*#__PURE__*/React.createElement(Spinner, {
    kind: "braille",
    tone: "accent"
  }), /*#__PURE__*/React.createElement("div", {
    style: {
      fontSize: 'var(--text-base)',
      fontWeight: 700,
      color: 'var(--text-bright)'
    }
  }, done ? 'Your app is ready' : 'Brewing your app…'), /*#__PURE__*/React.createElement("span", {
    style: {
      marginLeft: 'auto'
    }
  }, /*#__PURE__*/React.createElement(Badge, {
    tone: done ? 'success' : 'primary',
    dot: true
  }, done ? 'done' : 'installing'))), /*#__PURE__*/React.createElement(Progress, {
    value: pct,
    gradient: "var(--grad-neon-bar)",
    width: "100%"
  }), /*#__PURE__*/React.createElement("div", {
    style: {
      flex: 1,
      background: 'var(--bg-inset)',
      border: '1px solid var(--line-dim)',
      borderRadius: 'var(--radius-md)',
      padding: 14,
      fontFamily: 'var(--font-mono)',
      fontSize: 'var(--text-xs)',
      lineHeight: 1.9,
      overflow: 'hidden'
    }
  }, STEPS.slice(0, line).map((s, i) => /*#__PURE__*/React.createElement("div", {
    key: i,
    style: {
      color: 'var(--text-muted)'
    }
  }, /*#__PURE__*/React.createElement("span", {
    style: {
      color: 'var(--neon-mint)'
    }
  }, "\u2713"), " ", s)), !done && line < STEPS.length && /*#__PURE__*/React.createElement("div", {
    style: {
      color: 'var(--text-bright)'
    }
  }, /*#__PURE__*/React.createElement("span", {
    style: {
      color: 'var(--charm-pink)'
    }
  }, "\u2192"), " ", STEPS[line], /*#__PURE__*/React.createElement("span", {
    style: {
      color: 'var(--neon-cyan)'
    }
  }, "\u258D"))), done ? /*#__PURE__*/React.createElement(Dialog, {
    title: "Next steps",
    tone: "success",
    icon: "\u2713",
    actions: /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement(Button, {
      variant: "ghost",
      onClick: onRestart
    }, "Start over"), /*#__PURE__*/React.createElement(Button, {
      variant: "success",
      focused: true,
      onClick: onRestart
    }, "Run it \u2192"))
  }, /*#__PURE__*/React.createElement("div", {
    style: {
      fontFamily: 'var(--font-mono)'
    }
  }, /*#__PURE__*/React.createElement("span", {
    style: {
      color: 'var(--text-dim)'
    }
  }, "$ "), "cd ", config.name, /*#__PURE__*/React.createElement("br", null), /*#__PURE__*/React.createElement("span", {
    style: {
      color: 'var(--text-dim)'
    }
  }, "$ "), "go run .")) : /*#__PURE__*/React.createElement(KeyHint, {
    hints: [{
      keys: 'ctrl+c',
      label: 'cancel'
    }]
  }));
}
window.InstallScreen = InstallScreen;
})(); } catch (e) { __ds_ns.__errors.push({ path: "ui_kits/charm-cli/InstallScreen.jsx", error: String((e && e.message) || e) }); }

// ui_kits/charm-cli/MenuScreen.jsx
try { (() => {
// MenuScreen — the launcher. Pick what to brew from a Bubbles list.
const {
  List,
  KeyHint,
  Button,
  Badge
} = window.BubbleteaTUIDesignSystem_5c2f37;
const TEMPLATES = [{
  title: 'Bubble Tea App',
  desc: 'A full TUI, Elm-architecture starter',
  badge: '★ popular'
}, {
  title: 'Gum Script',
  desc: 'Glue shell scripts with interactive prompts'
}, {
  title: 'Glow Doc Site',
  desc: 'A markdown reader for your docs'
}, {
  title: 'Wish SSH App',
  desc: 'Serve your TUI over SSH'
}, {
  title: 'Soft Serve',
  desc: 'A self-hostable git server'
}];
function MenuScreen({
  selected,
  onSelect,
  onContinue
}) {
  return /*#__PURE__*/React.createElement("div", {
    style: {
      display: 'flex',
      flexDirection: 'column',
      gap: 16
    }
  }, /*#__PURE__*/React.createElement("div", {
    style: {
      display: 'flex',
      alignItems: 'center',
      gap: 14
    }
  }, /*#__PURE__*/React.createElement("span", {
    style: {
      fontSize: 'var(--text-lg)',
      lineHeight: 1
    },
    "aria-hidden": "true"
  }, "\uD83E\uDDCB"), /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("div", {
    style: {
      fontSize: 'var(--text-base)',
      fontWeight: 700,
      color: 'var(--text-bright)'
    }
  }, "What would you like to brew?"), /*#__PURE__*/React.createElement("div", {
    style: {
      fontSize: 'var(--text-xs)',
      color: 'var(--text-muted)',
      marginTop: 2
    }
  }, "Scaffold a new project from a Charm template.")), /*#__PURE__*/React.createElement("span", {
    style: {
      marginLeft: 'auto'
    }
  }, /*#__PURE__*/React.createElement(Badge, {
    tone: "info",
    variant: "outline"
  }, "charm v2.0.6"))), /*#__PURE__*/React.createElement("div", {
    style: {
      background: 'var(--bg-surface)',
      border: '1px solid var(--line-dim)',
      borderRadius: 'var(--radius-md)',
      padding: '8px 4px'
    }
  }, TEMPLATES.map((t, i) => /*#__PURE__*/React.createElement("div", {
    key: i,
    onClick: () => onSelect(i),
    onDoubleClick: onContinue,
    style: {
      cursor: 'pointer'
    }
  }, /*#__PURE__*/React.createElement(List, {
    selected: i === selected ? 0 : -1,
    items: [t],
    style: {
      pointerEvents: 'none'
    }
  })))), /*#__PURE__*/React.createElement("div", {
    style: {
      display: 'flex',
      alignItems: 'center',
      gap: 14
    }
  }, /*#__PURE__*/React.createElement(KeyHint, {
    hints: [{
      keys: '↑/↓',
      label: 'navigate'
    }, {
      keys: 'enter',
      label: 'select'
    }, {
      keys: 'q',
      label: 'quit'
    }]
  }), /*#__PURE__*/React.createElement("span", {
    style: {
      marginLeft: 'auto'
    }
  }, /*#__PURE__*/React.createElement(Button, {
    variant: "accent",
    focused: true,
    onClick: onContinue
  }, "Continue \u2192"))));
}
window.MenuScreen = MenuScreen;
})(); } catch (e) { __ds_ns.__errors.push({ path: "ui_kits/charm-cli/MenuScreen.jsx", error: String((e && e.message) || e) }); }

// ui_kits/glow/GlowReader.jsx
try { (() => {
// GlowReader — a two-pane markdown reader TUI (Glow + Glamour styling).
const {
  TerminalWindow,
  List,
  StatusBar,
  KeyHint,
  Badge
} = window.BubbleteaTUIDesignSystem_5c2f37;

// Tiny Glamour-style markdown renderer for a few known docs.
const md = {
  h1: t => /*#__PURE__*/React.createElement("div", {
    style: {
      fontSize: 'var(--text-base)',
      fontWeight: 700,
      color: 'var(--text-bright)',
      margin: '2px 0 10px'
    }
  }, /*#__PURE__*/React.createElement("span", {
    style: {
      color: 'var(--charm-pink)'
    }
  }, "# "), t),
  h2: t => /*#__PURE__*/React.createElement("div", {
    style: {
      fontSize: 'var(--text-base)',
      fontWeight: 700,
      color: 'var(--neon-lilac)',
      margin: '18px 0 8px'
    }
  }, /*#__PURE__*/React.createElement("span", {
    style: {
      color: 'var(--charm-purple)'
    }
  }, "## "), t),
  p: t => /*#__PURE__*/React.createElement("p", {
    style: {
      fontSize: 'var(--text-sm)',
      color: 'var(--text-body)',
      lineHeight: 1.75,
      margin: '0 0 10px'
    }
  }, t),
  li: t => /*#__PURE__*/React.createElement("div", {
    style: {
      fontSize: 'var(--text-sm)',
      color: 'var(--text-body)',
      lineHeight: 1.75,
      paddingLeft: 4
    }
  }, /*#__PURE__*/React.createElement("span", {
    style: {
      color: 'var(--neon-mint)'
    }
  }, "\u2022 "), t),
  code: t => /*#__PURE__*/React.createElement("div", {
    style: {
      background: 'var(--bg-inset)',
      border: '1px solid var(--line-dim)',
      borderRadius: 'var(--radius-sm)',
      padding: '10px 14px',
      margin: '8px 0 12px',
      fontSize: 'var(--text-xs)',
      lineHeight: 1.8,
      color: 'var(--neon-cyan)',
      whiteSpace: 'pre'
    }
  }, t),
  quote: t => /*#__PURE__*/React.createElement("div", {
    style: {
      borderLeft: '3px solid var(--charm-pink)',
      background: 'rgba(255,95,162,0.06)',
      padding: '8px 14px',
      margin: '8px 0 12px',
      fontSize: 'var(--text-sm)',
      color: 'var(--text-muted)',
      fontStyle: 'italic'
    }
  }, t)
};
const DOCS = [{
  title: 'README.md',
  desc: 'charmbracelet/bubbletea',
  badge: '4.2k ★',
  body: /*#__PURE__*/React.createElement(React.Fragment, null, md.h1('Bubble Tea 🫧'), md.p('The fun, functional and stateful way to build terminal apps. A Go framework based on The Elm Architecture.'), md.quote('Well-suited for simple and complex terminal applications, either inline, full-window, or a mix of both.'), md.h2('Init, Update, View'), md.li('Init — returns an initial command'), md.li('Update — handles messages, returns a new model'), md.li('View — renders the UI as a string'), md.code('func (m model) View() string {\n  return "brew some " + m.tea\n}'))
}, {
  title: 'lipgloss.md',
  desc: 'Style definitions for TUIs',
  badge: '',
  body: /*#__PURE__*/React.createElement(React.Fragment, null, md.h1('Lip Gloss 💄'), md.p('Style, format and layout tools for terminal applications. Set foreground and background colors, borders, padding, and alignment.'), md.h2('Rounded borders'), md.code('var style = lipgloss.NewStyle().\n  Border(lipgloss.RoundedBorder()).\n  BorderForeground(lipgloss.Color("#7D56F4"))'), md.li('AdaptiveColor for light/dark terminals'), md.li('JoinHorizontal / JoinVertical layouts'))
}, {
  title: 'gum.md',
  desc: 'Glamorous shell scripts',
  badge: 'new',
  body: /*#__PURE__*/React.createElement(React.Fragment, null, md.h1('Gum 🍬'), md.p('A tool for glamorous shell scripts. Use Gum to prompt, choose, filter, and confirm — all styled by Lip Gloss.'), md.code('gum choose "Bubble Tea" "Lip Gloss" "Glow"\ngum input --placeholder "your name"'), md.quote('Leverage the power of Bubbles and Lip Gloss without writing any Go.'))
}];
function GlowThemeToggle() {
  const [theme, setTheme] = React.useState(document.documentElement.dataset.theme === 'day' ? 'day' : 'night');
  const flip = () => {
    const t = theme === 'day' ? 'night' : 'day';
    document.documentElement.dataset.theme = t;
    localStorage.setItem('btds-theme', t);
    setTheme(t);
  };
  return /*#__PURE__*/React.createElement("button", {
    onClick: flip,
    style: {
      display: 'inline-flex',
      alignItems: 'center',
      gap: 8,
      padding: '7px 16px',
      fontFamily: 'var(--font-mono)',
      fontSize: 'var(--text-xs)',
      fontWeight: 700,
      color: 'var(--text-body)',
      background: 'var(--bg-surface)',
      border: '1.5px solid var(--line)',
      borderRadius: 'var(--radius-pill)',
      cursor: 'pointer'
    }
  }, theme === 'day' ? '☾ night' : '☀ day');
}
function GlowReader() {
  const [doc, setDoc] = React.useState(0);
  const active = DOCS[doc];
  return /*#__PURE__*/React.createElement("div", {
    style: {
      width: 'min(1000px, 95vw)'
    }
  }, /*#__PURE__*/React.createElement("div", {
    style: {
      display: 'flex',
      justifyContent: 'flex-end',
      marginBottom: 14
    }
  }, /*#__PURE__*/React.createElement(GlowThemeToggle, null)), /*#__PURE__*/React.createElement(TerminalWindow, {
    title: "glow \u2014 stash",
    glow: "cyan",
    statusBar: /*#__PURE__*/React.createElement(StatusBar, {
      segments: [{
        label: 'READING',
        tone: 'info'
      }, {
        label: active.title,
        tone: 'plain',
        grow: true
      }, {
        label: `${doc + 1}/${DOCS.length}`,
        tone: 'muted'
      }, {
        label: 'markdown',
        tone: 'accent'
      }]
    })
  }, /*#__PURE__*/React.createElement("div", {
    style: {
      display: 'grid',
      gridTemplateColumns: '280px 1fr',
      gap: 24,
      minHeight: 440
    }
  }, /*#__PURE__*/React.createElement("div", {
    style: {
      borderRight: '1px solid var(--line-dim)',
      paddingRight: 16
    }
  }, /*#__PURE__*/React.createElement("div", {
    style: {
      display: 'flex',
      alignItems: 'center',
      gap: 8,
      marginBottom: 12
    }
  }, /*#__PURE__*/React.createElement("span", {
    style: {
      fontSize: 'var(--text-md)',
      lineHeight: 1
    },
    "aria-hidden": "true"
  }, "\uD83E\uDDCB"), /*#__PURE__*/React.createElement("span", {
    style: {
      fontSize: 'var(--text-xs)',
      color: 'var(--text-muted)',
      letterSpacing: '0.06em'
    }
  }, "DOCUMENTS"), /*#__PURE__*/React.createElement("span", {
    style: {
      marginLeft: 'auto'
    }
  }, /*#__PURE__*/React.createElement(Badge, {
    tone: "muted",
    variant: "outline"
  }, DOCS.length))), DOCS.map((d, i) => /*#__PURE__*/React.createElement("div", {
    key: i,
    onClick: () => setDoc(i),
    style: {
      cursor: 'pointer'
    }
  }, /*#__PURE__*/React.createElement(List, {
    selected: i === doc ? 0 : -1,
    items: [{
      title: d.title,
      desc: d.desc,
      badge: d.badge
    }],
    style: {
      pointerEvents: 'none'
    }
  }))), /*#__PURE__*/React.createElement("div", {
    style: {
      marginTop: 16
    }
  }, /*#__PURE__*/React.createElement(KeyHint, {
    hints: [{
      keys: '↑/↓',
      label: 'browse'
    }, {
      keys: 'enter',
      label: 'read'
    }]
  }))), /*#__PURE__*/React.createElement("div", {
    style: {
      paddingRight: 6,
      maxHeight: 400,
      overflow: 'auto'
    }
  }, active.body))));
}
ReactDOM.createRoot(document.getElementById('root')).render(/*#__PURE__*/React.createElement(GlowReader, null));
})(); } catch (e) { __ds_ns.__errors.push({ path: "ui_kits/glow/GlowReader.jsx", error: String((e && e.message) || e) }); }

__ds_ns.Badge = __ds_scope.Badge;

__ds_ns.List = __ds_scope.List;

__ds_ns.Table = __ds_scope.Table;

__ds_ns.Tabs = __ds_scope.Tabs;

__ds_ns.Dialog = __ds_scope.Dialog;

__ds_ns.KeyHint = __ds_scope.KeyHint;

__ds_ns.Progress = __ds_scope.Progress;

__ds_ns.Spinner = __ds_scope.Spinner;

__ds_ns.Button = __ds_scope.Button;

__ds_ns.Checkbox = __ds_scope.Checkbox;

__ds_ns.TextInput = __ds_scope.TextInput;

__ds_ns.Toggle = __ds_scope.Toggle;

__ds_ns.Kbd = __ds_scope.Kbd;

__ds_ns.StatusBar = __ds_scope.StatusBar;

__ds_ns.TerminalWindow = __ds_scope.TerminalWindow;

})();
