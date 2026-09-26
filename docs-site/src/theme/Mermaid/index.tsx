/**
 * Mermaid wrapper (swizzled in wrap mode around @theme-original/Mermaid).
 *
 * Adds, around the stock renderer:
 *
 *   - pan and zoom: toolbar buttons, Ctrl/Cmd + wheel (and trackpad pinch,
 *     which browsers report as Ctrl + wheel), drag to pan once zoomed in;
 *   - "Expand": the diagram re-rendered in a modal <dialog> sized to the
 *     window, with the same controls, where a plain wheel zooms. Esc and a
 *     backdrop click close it, and focus returns to the Expand button;
 *   - deferred loading: nothing is rendered, and the ~650 KB Mermaid chunk is
 *     not requested, until the diagram is near the viewport. Most ADR pages
 *     carry one diagram at the very bottom ("Related Artifacts");
 *   - a readable failure: the Mermaid library is imported here first, so a
 *     network failure (after the retries in clientModules/chunkLoadRetry)
 *     shows a retry button and the diagram source instead of a crashed block.
 *     The stock component memoizes its import, so a failure inside it could
 *     only be cleared by a reload.
 *
 * Everything touching window/document runs in effects, so SSR renders only
 * the static frame.
 */
import React, {useCallback, useEffect, useRef, useState, type ReactNode} from 'react';
import Panzoom, {type PanzoomObject} from '@panzoom/panzoom';
import OriginalMermaid from '@theme-original/Mermaid';
import type MermaidType from '@theme/Mermaid';
import type {WrapperProps} from '@docusaurus/types';

type Props = WrapperProps<typeof MermaidType>;

const MAX_SCALE = 8;
const MIN_SCALE = 0.25;
const ZOOM_STEP = 0.25;

// ---------------------------------------------------------------------------
// Loading
// ---------------------------------------------------------------------------

// True once the element is within ~one screen of the viewport. Without
// IntersectionObserver (old browsers, some test runners) render immediately.
function useNearViewport(ref: React.RefObject<HTMLElement | null>): boolean {
  const [near, setNear] = useState(false);
  useEffect(() => {
    if (near) return undefined;
    const el = ref.current;
    if (!el || typeof IntersectionObserver === 'undefined') {
      setNear(true);
      return undefined;
    }
    const io = new IntersectionObserver(
      (entries) => {
        if (entries.some((e) => e.isIntersecting)) {
          setNear(true);
          io.disconnect();
        }
      },
      {rootMargin: '100% 0px'},
    );
    io.observe(el);
    return () => io.disconnect();
  }, [near, ref]);
  return near;
}

type LibraryState = 'idle' | 'loading' | 'ready' | 'failed';

// Imports the Mermaid library ahead of the stock component. Same module, so
// once this resolves the stock component's own import is served from the
// module cache.
//
// Every diagram on a page imports the same chunk, so when one "Try again"
// succeeds the others that failed are told to try again too.
const libraryLoaded = new Set<() => void>();

function useMermaidLibrary(enabled: boolean): [LibraryState, () => void] {
  const [state, setState] = useState<LibraryState>('idle');
  const [attempt, setAttempt] = useState(0);
  useEffect(() => {
    if (!enabled) return undefined;
    let live = true;
    setState('loading');
    import('mermaid').then(
      () => {
        if (live) setState('ready');
        libraryLoaded.forEach((notify) => notify());
      },
      () => live && setState('failed'),
    );
    return () => {
      live = false;
    };
  }, [enabled, attempt]);
  useEffect(() => {
    if (state !== 'failed') return undefined;
    const notify = () => setAttempt((n) => n + 1);
    libraryLoaded.add(notify);
    return () => {
      libraryLoaded.delete(notify);
    };
  }, [state]);
  const retry = useCallback(() => setAttempt((n) => n + 1), []);
  return [state, retry];
}

// ---------------------------------------------------------------------------
// Pan / zoom
// ---------------------------------------------------------------------------

type WheelMode = 'modifier' | 'always';

// Binds Panzoom to `stage`, whose parent is the clipping viewport. Inline
// (`modifier`), the page keeps scrolling under a plain wheel and dragging is
// off until the reader zooms in, so a diagram never traps the page. In the
// dialog (`always`) the wheel zooms and dragging always pans.
function usePanzoom(
  stageRef: React.RefObject<HTMLDivElement | null>,
  active: boolean,
  wheel: WheelMode,
): React.RefObject<PanzoomObject | null> {
  const pzRef = useRef<PanzoomObject | null>(null);
  useEffect(() => {
    const stage = stageRef.current;
    const viewport = stage?.parentElement;
    if (!active || !stage || !viewport) return undefined;

    const inline = wheel === 'modifier';
    const pz = Panzoom(stage, {
      canvas: true,
      maxScale: MAX_SCALE,
      minScale: MIN_SCALE,
      step: ZOOM_STEP,
      disablePan: inline,
      cursor: inline ? 'auto' : 'grab',
      touchAction: inline ? 'pan-y' : 'none',
    });

    const onZoom = (e: Event) => {
      if (!inline) return;
      const {scale} = (e as CustomEvent<{scale: number}>).detail;
      const zoomed = scale > 1.001;
      pz.setOptions({disablePan: !zoomed, cursor: zoomed ? 'grab' : 'auto'});
      viewport.dataset.zoomed = zoomed ? 'true' : 'false';
    };
    const onWheel = (e: WheelEvent) => {
      if (inline && !e.ctrlKey && !e.metaKey) return;
      pz.zoomWithWheel(e); // calls preventDefault
    };
    stage.addEventListener('panzoomzoom', onZoom);
    stage.addEventListener('panzoomreset', onZoom);
    viewport.addEventListener('wheel', onWheel, {passive: false});
    pzRef.current = pz;
    return () => {
      stage.removeEventListener('panzoomzoom', onZoom);
      stage.removeEventListener('panzoomreset', onZoom);
      viewport.removeEventListener('wheel', onWheel);
      pz.destroy();
      pzRef.current = null;
    };
  }, [active, stageRef, wheel]);
  return pzRef;
}

// ---------------------------------------------------------------------------
// UI pieces
// ---------------------------------------------------------------------------

function Icon({d}: {d: string}): ReactNode {
  return (
    <svg viewBox="0 0 16 16" width="14" height="14" aria-hidden="true" focusable="false">
      <path d={d} fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round" />
    </svg>
  );
}

const ICONS = {
  zoomIn: 'M7 3v8M3 7h8M11.5 11.5 14 14',
  zoomOut: 'M3 7h8M11.5 11.5 14 14',
  reset: 'M2.5 8a5.5 5.5 0 1 0 1.6-3.9M2.5 2.5v2.6h2.6',
  expand: 'M9.5 2.5h4v4M6.5 13.5h-4v-4M13.5 2.5 9 7M2.5 13.5 7 9',
  close: 'M3.5 3.5l9 9M12.5 3.5l-9 9',
};

function ToolButton({
  label,
  icon,
  onClick,
  buttonRef,
}: {
  label: string;
  icon: keyof typeof ICONS;
  onClick: () => void;
  buttonRef?: React.Ref<HTMLButtonElement>;
}): ReactNode {
  return (
    <button
      ref={buttonRef}
      type="button"
      className="diagram__button"
      aria-label={label}
      title={label}
      onClick={onClick}>
      <Icon d={ICONS[icon]} />
    </button>
  );
}

function ZoomButtons({pz}: {pz: React.RefObject<PanzoomObject | null>}): ReactNode {
  return (
    <>
      <ToolButton label="Zoom in" icon="zoomIn" onClick={() => pz.current?.zoomIn()} />
      <ToolButton label="Zoom out" icon="zoomOut" onClick={() => pz.current?.zoomOut()} />
      <ToolButton label="Reset zoom" icon="reset" onClick={() => pz.current?.reset()} />
    </>
  );
}

function LoadFailure({value, onRetry}: {value: string; onRetry: () => void}): ReactNode {
  return (
    <div className="diagram__failure" role="status">
      <p>This diagram did not load — the network request failed or timed out.</p>
      <p className="diagram__failure-actions">
        <button type="button" className="button button--sm button--primary" onClick={onRetry}>
          Try again
        </button>
        <button type="button" className="button button--sm button--secondary" onClick={() => window.location.reload()}>
          Reload page
        </button>
      </p>
      <details>
        <summary>Diagram source</summary>
        <pre>
          <code>{value}</code>
        </pre>
      </details>
    </div>
  );
}

function ExpandedDiagram({
  props,
  onClosed,
}: {
  props: Props;
  onClosed: () => void;
}): ReactNode {
  const dialogRef = useRef<HTMLDialogElement>(null);
  const stageRef = useRef<HTMLDivElement>(null);
  const [open, setOpen] = useState(false);
  const pz = usePanzoom(stageRef, open, 'always');

  useEffect(() => {
    const dialog = dialogRef.current;
    if (!dialog) return;
    if (!dialog.open) dialog.showModal();
    setOpen(true);
  }, []);

  const close = () => dialogRef.current?.close();
  // Where the press began. A pan that starts on the diagram and is released
  // over the backdrop produces a click targeting the <dialog>, too.
  const pressedBackdrop = useRef(false);

  return (
    // Esc fires `cancel` then `close`; both routes end in onClose.
    // A click whose target is the <dialog> itself landed on the backdrop:
    // the content fills the box, so any click inside hits a child.
    // eslint-disable-next-line jsx-a11y/click-events-have-key-events, jsx-a11y/no-noninteractive-element-interactions
    <dialog
      ref={dialogRef}
      className="diagram-dialog"
      aria-label="Expanded diagram"
      onClose={onClosed}
      onPointerDown={(e) => {
        pressedBackdrop.current = e.target === e.currentTarget;
      }}
      onClick={(e) => {
        if (e.target === e.currentTarget && pressedBackdrop.current) close();
      }}>
      <div className="diagram-dialog__frame">
        <div className="diagram__toolbar">
          <span className="diagram__hint">Scroll to zoom · drag to pan · Esc to close</span>
          <ZoomButtons pz={pz} />
          <ToolButton label="Close expanded diagram" icon="close" onClick={close} />
        </div>
        <div className="diagram__viewport diagram-dialog__viewport">
          <div ref={stageRef} className="diagram__stage">
            <OriginalMermaid {...props} />
          </div>
        </div>
      </div>
    </dialog>
  );
}

// ---------------------------------------------------------------------------
// Wrapper
// ---------------------------------------------------------------------------

export default function MermaidWrapper(props: Props): ReactNode {
  const figureRef = useRef<HTMLElement>(null);
  const stageRef = useRef<HTMLDivElement>(null);
  const expandRef = useRef<HTMLButtonElement>(null);
  const [expanded, setExpanded] = useState(false);

  const near = useNearViewport(figureRef);
  const [library, retry] = useMermaidLibrary(near);
  const ready = library === 'ready';
  const pz = usePanzoom(stageRef, ready, 'modifier');

  const onClosed = useCallback(() => {
    setExpanded(false);
    expandRef.current?.focus();
  }, []);

  return (
    <figure ref={figureRef} className="diagram">
      {ready && (
        <div className="diagram__toolbar">
          <span className="diagram__hint">Ctrl/⌘ + scroll to zoom</span>
          <ZoomButtons pz={pz} />
          <ToolButton
            label="Expand diagram"
            icon="expand"
            buttonRef={expandRef}
            onClick={() => setExpanded(true)}
          />
        </div>
      )}
      {library === 'failed' ? (
        <LoadFailure value={props.value} onRetry={retry} />
      ) : (
        <div className="diagram__viewport" data-zoomed="false">
          <div ref={stageRef} className="diagram__stage">
            {ready ? (
              <OriginalMermaid {...props} />
            ) : (
              <div className="diagram__placeholder" aria-hidden="true" />
            )}
          </div>
        </div>
      )}
      {expanded && <ExpandedDiagram props={props} onClosed={onClosed} />}
    </figure>
  );
}
