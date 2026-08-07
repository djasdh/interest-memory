"""interest-memory installer — curses UI components.

A faithful adaptation of Hermes' `curses_ui.py` (Nous Research) so the
installer feels like the platform's own plugin pickers: green cursor rows,
yellow titles, dim hints, `↑↓/jk` navigation, SPACE toggling, ENTER confirm,
`/` fuzzy search, ESC/q to go back one wizard step (Ctrl+C aborts cleanly).

When curses is unavailable (e.g. stock Windows Python) or stdin isn't a TTY,
every picker degrades to a numbered text fallback (the same strategy Hermes
uses), so the installer stays usable everywhere.
"""
from __future__ import annotations

import sys
import threading
import time
from typing import Any, Callable, Iterable, Optional, Set

# ---------------------------------------------------------------------------
# Key normalization (ported from Hermes read_menu_key / _decode_menu_key).
# ---------------------------------------------------------------------------
NAV_UP = "up"
NAV_DOWN = "down"
NAV_SELECT = "select"
NAV_TOGGLE = "toggle"
NAV_CANCEL = "cancel"
NAV_ABORT = "abort"
NAV_NONE = "none"


class GoBack(BaseException):
    """User pressed ESC or q: leave the current UI, go back one wizard step.

    Inherits BaseException (like KeyboardInterrupt) so generic ``except
    Exception:`` fallback handlers never swallow it.
    """


def read_menu_key(stdscr) -> str:
    """Read one keypress and normalize it to a menu action."""
    return _decode_menu_key(stdscr, stdscr.getch())


def _prepare_curses() -> None:
    """Tune curses globals before initscr: keep ESC/arrow-key response snappy.

    ncurses' default ESC delay is 1000ms, so pressing ESC takes a full second
    before the app sees it. Lower it (and keep a short peek in
    _decode_menu_key) so ESC/q back-navigation feels instant.
    """
    import curses

    try:
        curses.set_escdelay(25)  # ms (default is 1000)
    except Exception:
        pass  # very old curses build: accept the lag


def _decode_menu_key(stdscr, key: int) -> str:
    import curses

    if key in (curses.KEY_UP, ord("k")):
        return NAV_UP
    if key in (curses.KEY_DOWN, ord("j")):
        return NAV_DOWN
    if key in (curses.KEY_ENTER, 10, 13):
        return NAV_SELECT
    if key == ord(" "):
        return NAV_TOGGLE
    if key == ord("q"):
        return NAV_CANCEL
    if key == 3:  # Ctrl+C — abort the whole wizard, not just this menu
        return NAV_ABORT

    if key == 27:  # ESC — lone ESC backs up; else decode CSI/SS3 sequences.
        # ncurses already waited ESCDELAY (see _prepare_curses) to classify the
        # key; a short peek only catches split/rapid sequences.
        try:
            stdscr.timeout(20)
            nxt = stdscr.getch()
        finally:
            stdscr.timeout(-1)
        if nxt in (-1, 27):  # lone ESC (or a fast double-ESC)
            return NAV_CANCEL
        if nxt in (ord("["), ord("O")):
            final = stdscr.getch()
            if final in (ord("A"), ord("k")):
                return NAV_UP
            if final in (ord("B"), ord("j")):
                return NAV_DOWN
            while 0x20 <= final <= 0x3F:  # consume CSI tail bytes
                final = stdscr.getch()
            return NAV_NONE
        return NAV_NONE

    return NAV_NONE


def flush_stdin() -> None:
    """Drain stray bytes from stdin so the next input() isn't corrupted."""
    try:
        if not sys.stdin.isatty():
            return
        import termios
        termios.tcflush(sys.stdin, termios.TCIFLUSH)
    except Exception:
        pass


# ---------------------------------------------------------------------------
# Fuzzy matching (ported from Hermes' TS-scorer port).
# ---------------------------------------------------------------------------
_WORD_BOUNDARY = frozenset("-_/. ")


def _is_boundary(target: str, index: int) -> bool:
    if index == 0:
        return True
    prev = target[index - 1]
    if prev in _WORD_BOUNDARY:
        return True
    cur = target[index]
    return prev == prev.lower() and cur != cur.lower() and cur == cur.upper()


def _token_score(orig: str, lower: str, token: str) -> Optional[float]:
    score = 0.0
    prev = -1
    search_from = 0
    positions = []

    for ch in token:
        idx = lower.find(ch, search_from)
        if idx < 0:
            return None
        positions.append(idx)
        score += 1
        if prev >= 0 and idx == prev + 1:
            score += 5
        elif prev >= 0:
            score -= min(idx - prev - 1, 3)
        if _is_boundary(orig, idx):
            score += 3
        if idx == 0:
            score += 5
        prev = idx
        search_from = idx + 1

    if positions and positions[0] == 0 and positions[-1] == len(positions) - 1:
        score += 8
    if lower == token:
        score += 20
    score -= len(lower) * 0.01
    return score


def _fuzzy_score(label: str, query: str) -> Optional[float]:
    lower = label.lower()
    tokens = query.lower().split()
    if not tokens:
        return 0.0
    total = 0.0
    for token in tokens:
        ts = _token_score(label, lower, token)
        if ts is None:
            return None
        total += ts
    return total


def _filter_indices(items: list[str], query: str) -> list[int]:
    q = query.strip()
    if not q:
        return list(range(len(items)))
    scored = []
    for i, label in enumerate(items):
        s = _fuzzy_score(label, q)
        if s is not None:
            scored.append((i, s))
    scored.sort(key=lambda pair: (-pair[1], pair[0]))
    return [i for i, _ in scored]


# ---------------------------------------------------------------------------
# Shared event loop (adapted from Hermes _run_curses_menu).
# ---------------------------------------------------------------------------
_KEEP = object()


def _run_menu(
    *,
    item_count: int,
    draw_header,
    draw_row,
    on_action,
    searchable: bool = False,
    search_labels: Optional[list[str]] = None,
    fallback,
    cancel_value,
    initial_cursor: int = 0,
):
    """Drive a curses single-/multi-select loop. Returns the resolver value."""
    if not sys.stdin.isatty():
        return cancel_value

    use_search = searchable and search_labels is not None and len(search_labels) == item_count

    try:
        import curses
    except Exception:
        return fallback()
    _prepare_curses()

    result_holder = [_KEEP]
    labels = search_labels if (use_search and search_labels is not None) else []

    def _draw(stdscr):
        curses.curs_set(0)
        if curses.has_colors():
            curses.start_color()
            curses.use_default_colors()
            curses.init_pair(1, curses.COLOR_GREEN, -1)
            curses.init_pair(2, curses.COLOR_YELLOW, -1)
        cursor = min(initial_cursor, item_count - 1) if item_count > 0 else 0
        scroll_offset = 0
        search_query = ""
        search_active = False

        def reconcile(filtered):
            nonlocal cursor
            if not filtered:
                return cursor, 0
            if cursor not in filtered:
                cursor = filtered[0]
            return cursor, filtered.index(cursor)

        while True:
            stdscr.clear()
            max_y, max_x = stdscr.getmaxyx()

            filtered = _filter_indices(labels, search_query) if use_search else list(range(item_count))
            cursor, cursor_pos = reconcile(filtered)

            items_start = draw_header(stdscr, max_y, max_x, search_query, search_active)
            visible_rows = max(1, max_y - items_start - 1)

            if cursor_pos < scroll_offset:
                scroll_offset = cursor_pos
            elif cursor_pos >= scroll_offset + visible_rows:
                scroll_offset = cursor_pos - visible_rows + 1
            scroll_offset = max(0, min(scroll_offset, max(0, len(filtered) - visible_rows)))

            if use_search and search_query and not filtered:
                try:
                    stdscr.addnstr(items_start, 0, "  No matches", max_x - 1, curses.A_DIM)
                except curses.error:
                    pass

            for draw_i in range(scroll_offset, min(len(filtered), scroll_offset + visible_rows)):
                i = filtered[draw_i]
                y = draw_i - scroll_offset + items_start
                if y >= max_y - 1:
                    break
                draw_row(stdscr, y, i, i == cursor, max_x)

            stdscr.refresh()

            if use_search:
                key = stdscr.getch()
                if search_active:
                    handled, confirm, changed, new_query = _handle_search_key(
                        curses, key, search_query
                    )
                    if changed:
                        search_query = new_query
                        scroll_offset = 0
                        cursor, cursor_pos = reconcile(_filter_indices(search_labels, search_query))
                    if confirm:
                        if filtered:
                            outcome = on_action(NAV_SELECT, cursor)
                            if outcome is not _KEEP:
                                result_holder[0] = outcome
                                return
                        continue
                    if handled:
                        continue
                    action = _decode_menu_key(stdscr, key)
                elif key == ord("/"):
                    search_active = True
                    continue
                else:
                    action = _decode_menu_key(stdscr, key)
            else:
                action = read_menu_key(stdscr)

            if action == NAV_UP:
                if filtered:
                    cursor = filtered[(cursor_pos - 1) % len(filtered)]
            elif action == NAV_DOWN:
                if filtered:
                    cursor = filtered[(cursor_pos + 1) % len(filtered)]
            elif action == NAV_ABORT:
                raise KeyboardInterrupt  # Ctrl+C: abort the whole wizard
            elif action == NAV_CANCEL:
                raise GoBack()  # ESC/q: go back to the previous step
            elif action in (NAV_SELECT, NAV_TOGGLE):
                if action == NAV_SELECT and use_search and not filtered:
                    continue
                outcome = on_action(action, cursor)
                if outcome is not _KEEP:
                    result_holder[0] = outcome
                    return

    try:
        curses.wrapper(_draw)
        flush_stdin()
        return result_holder[0] if result_holder[0] is not _KEEP else cancel_value
    except (KeyboardInterrupt, GoBack):
        raise  # propagate: Ctrl+C exits the wizard; ESC/q steps back
    except Exception:
        return fallback()


def _handle_search_key(curses, key: int, query: str):
    """Return (handled, confirm, changed, new_query)."""
    if key == 27:  # ESC stops search and clears the query
        return True, False, bool(query), ""
    if key in (curses.KEY_BACKSPACE, 127, 8):
        return True, False, True, query[:-1]
    if key == 21:  # Ctrl+U
        return True, False, True, ""
    if key in (curses.KEY_ENTER, 10, 13):
        return True, True, False, query
    if 32 <= key < 127:
        return True, False, True, query + chr(key)
    return False, False, False, query


# ---------------------------------------------------------------------------
# Public pickers.
# ---------------------------------------------------------------------------
def pick_radio(
    title: str,
    items: list[str],
    selected: int = 0,
    *,
    searchable: bool = True,
    description: str | None = None,
    cancel_returns: Optional[int] = None,
) -> int:
    """Single-select radio list (curses), numbered fallback otherwise."""
    if cancel_returns is None:
        cancel_returns = selected
    n = len(items)

    def draw_header(stdscr, max_y, max_x, search_query="", search_active=False):
        import curses
        row = 0
        try:
            hattr = curses.A_BOLD
            if curses.has_colors():
                hattr |= curses.color_pair(2)
            stdscr.addnstr(row, 0, title, max_x - 1, hattr)
            row += 1
            if description:
                for dline in description.splitlines():
                    if row >= max_y - 1:
                        break
                    stdscr.addnstr(row, 0, dline, max_x - 1, curses.A_NORMAL)
                    row += 1
            if search_active:
                hint = f"  Search: {search_query}▎  BACKSPACE 删除  Ctrl+U 清空  ESC 停止 / BACKSPACE edit  Ctrl+U clear  ESC stop"
            elif searchable:
                hint = "  ↑↓ 导航  ENTER/SPACE 选择  / 搜索  ESC/q 返回上一步 / ↑↓ navigate  ENTER/SPACE select  / search  ESC/q back"
            else:
                hint = "  ↑↓ 导航  ENTER/SPACE 选择  ESC/q 返回上一步 / ↑↓ navigate  ENTER/SPACE select  ESC/q back"
            stdscr.addnstr(row, 0, hint, max_x - 1, curses.A_DIM)
            row += 1
        except curses.error:
            pass
        return row + 1

    def draw_row(stdscr, y, i, is_cursor, max_x):
        import curses
        radio = "\u25cf" if i == selected else "\u25cb"
        arrow = "\u2192" if is_cursor else " "
        line = f" {arrow} ({radio}) {items[i]}"
        attr = curses.A_NORMAL
        if is_cursor:
            attr = curses.A_BOLD
            if curses.has_colors():
                attr |= curses.color_pair(1)
        try:
            stdscr.addnstr(y, 0, line, max_x - 1, attr)
        except curses.error:
            pass

    def on_action(action, cursor):
        if action in (NAV_SELECT, NAV_TOGGLE):
            return cursor
        return cancel_returns

    return _run_menu(
        item_count=n,
        draw_header=draw_header,
        draw_row=draw_row,
        on_action=on_action,
        searchable=searchable,
        search_labels=list(items) if searchable else None,
        fallback=lambda: _radio_fallback(title, items, selected, cancel_returns),
        cancel_value=cancel_returns,
        initial_cursor=min(selected, n - 1) if n > 0 else 0,
    )


def pick_checklist(
    title: str,
    items: list[str],
    selected: Optional[Set[int]] = None,
    *,
    status_fn: Optional[Callable[[Set[int]], str]] = None,
) -> Set[int]:
    """Multi-select checklist (curses), numbered fallback otherwise."""
    if selected is None:
        selected = set()
    chosen = set(selected)
    cancel_returns = set(selected)

    def draw_header(stdscr, max_y, max_x, search_query="", search_active=False):
        import curses
        try:
            hattr = curses.A_BOLD
            if curses.has_colors():
                hattr |= curses.color_pair(2)
            stdscr.addnstr(0, 0, title, max_x - 1, hattr)
            stdscr.addnstr(1, 0, "  ↑↓ 导航  SPACE 切换  ENTER 确认  ESC/q 返回上一步 / ↑↓ navigate  SPACE toggle  ENTER confirm  ESC/q back", max_x - 1, curses.A_DIM)
        except curses.error:
            pass
        return 3

    def draw_row(stdscr, y, i, is_cursor, max_x):
        import curses
        check = "\u2713" if i in chosen else " "
        arrow = "\u2192" if is_cursor else " "
        line = f" {arrow} [{check}] {items[i]}"
        attr = curses.A_NORMAL
        if is_cursor:
            attr = curses.A_BOLD
            if curses.has_colors():
                attr |= curses.color_pair(1)
        try:
            stdscr.addnstr(y, 0, line, max_x - 1, attr)
        except curses.error:
            pass

    def draw_footer(stdscr, max_y, max_x):
        if not status_fn:
            return
        import curses
        try:
            text = status_fn(chosen)
            if text:
                sx = max(0, max_x - len(text) - 1)
                stdscr.addnstr(max_y - 1, sx, text, max_x - sx - 1, curses.A_DIM)
        except curses.error:
            pass

    def on_action(action, cursor):
        if action == NAV_TOGGLE:
            chosen.symmetric_difference_update({cursor})
            return _KEEP
        if action == NAV_SELECT:
            return set(chosen)
        return cancel_returns

    return _run_menu(
        item_count=len(items),
        draw_header=draw_header,
        draw_row=draw_row,
        on_action=on_action,
        fallback=lambda: _checklist_fallback(title, items, selected, cancel_returns, status_fn),
        cancel_value=cancel_returns,
    )


def pick_single(title: str, items: list[str], default_index: int = 0, *, cancel_label: str = "Cancel") -> Optional[int]:
    """Single-select menu with a trailing Cancel row. None on cancel."""
    all_items = list(items) + [cancel_label]
    cancel_idx = len(items)

    def draw_header(stdscr, max_y, max_x, search_query="", search_active=False):
        import curses
        try:
            hattr = curses.A_BOLD
            if curses.has_colors():
                hattr |= curses.color_pair(2)
            stdscr.addnstr(0, 0, title, max_x - 1, hattr)
            stdscr.addnstr(1, 0, "  ↑↓ 导航  ENTER 确认  ESC/q 返回上一步 / ↑↓ navigate  ENTER confirm  ESC/q back", max_x - 1, curses.A_DIM)
        except curses.error:
            pass
        return 3

    def draw_row(stdscr, y, i, is_cursor, max_x):
        import curses
        arrow = "\u2192" if is_cursor else " "
        line = f" {arrow} {all_items[i]}"
        attr = curses.A_NORMAL
        if is_cursor:
            attr = curses.A_BOLD
            if curses.has_colors():
                attr |= curses.color_pair(1)
        try:
            stdscr.addnstr(y, 0, line, max_x - 1, attr)
        except curses.error:
            pass

    def on_action(action, cursor):
        if action == NAV_SELECT:
            return None if cursor >= cancel_idx else cursor
        if action == NAV_CANCEL:
            return None
        return _KEEP

    return _run_menu(
        item_count=len(all_items),
        draw_header=draw_header,
        draw_row=draw_row,
        on_action=on_action,
        fallback=lambda: _single_fallback(title, all_items, cancel_idx),
        cancel_value=None,
    )


# ---------------------------------------------------------------------------
# Text / confirm / message helpers (work everywhere).
# ---------------------------------------------------------------------------
def ask_input(prompt: str, default: str = "") -> str:
    """Read a line of input (curses editor when possible); else plain input.

    Empty input returns *default*.
    """
    if sys.stdin.isatty() and _curses_available():
        return _ask_input_curses(prompt, default)
    if not sys.stdin.isatty():
        return default
    try:
        val = input(prompt + " ").strip()
    except (EOFError, KeyboardInterrupt):
        return default
    return val or default


def ask_confirm(text: str, default: bool = True) -> bool:
    """Yes/no prompt (curses Yes/No picker when possible); else plain input.

    Non-TTY or empty input returns *default*.
    """
    if sys.stdin.isatty() and _curses_available():
        return _ask_confirm_curses(text, default)
    if not sys.stdin.isatty():
        return default
    suffix = " [Y/n]" if default else " [y/N]"
    try:
        val = input(text + suffix + " ").strip().lower()
    except (EOFError, KeyboardInterrupt):
        return default
    if not val:
        return default
    return val in ("y", "yes")


def _curses_available() -> bool:
    try:
        import curses  # noqa: F401
        return True
    except Exception:
        return False


def _ask_input_curses(prompt: str, default: str = "") -> str:
    """curses single-line input editor. Enter confirms; ESC/q steps back."""
    try:
        import curses
        _prepare_curses()

        result = [default]

        def _draw(stdscr):
            curses.curs_set(1)
            max_y, max_x = stdscr.getmaxyx()
            buffer = list(default)
            pos = len(buffer)
            while True:
                stdscr.clear()
                try:
                    title = "  " + prompt
                    stdscr.addnstr(0, 0, title, max_x - 1, curses.A_BOLD)
                    shown = "".join(buffer)
                    stdscr.addnstr(1, 0, "  " + shown, max_x - 1)
                    # draw cursor at the current position
                    cx = min(2 + pos, max_x - 1)
                    stdscr.move(1, cx)
                    hint = "  Enter 确认  ESC/q 返回上一步 / Enter confirm  ESC/q back"
                    stdscr.addnstr(2, 0, hint, max_x - 1, curses.A_DIM)
                except curses.error:
                    pass
                stdscr.refresh()
                key = stdscr.getch()
                if key in (curses.KEY_ENTER, 10, 13):
                    result[0] = "".join(buffer).strip() or default
                    return
                if key == 27:
                    raise GoBack()  # ESC/q: back to the previous step
                if key == 3:  # Ctrl+C — cancel and propagate a clean exit
                    raise KeyboardInterrupt
                if key in (curses.KEY_BACKSPACE, 127, 8) and pos > 0:
                    buffer.pop(pos - 1)
                    pos -= 1
                elif key == 21:  # Ctrl+U
                    buffer.clear()
                    pos = 0
                elif 32 <= key < 127:
                    buffer.insert(pos, chr(key))
                    pos += 1
                elif key == curses.KEY_LEFT and pos > 0:
                    pos -= 1
                elif key == curses.KEY_RIGHT and pos < len(buffer):
                    pos += 1

        curses.wrapper(_draw)
        flush_stdin()
        return result[0]
    except Exception:
        # Fall back to plain input (never block on curses errors).
        if not sys.stdin.isatty():
            return default
        try:
            val = input(prompt + " ").strip()
        except (EOFError, KeyboardInterrupt):
            return default
        return val or default


def _ask_confirm_curses(text: str, default: bool = True) -> bool:
    """curses Yes/No picker. Enter confirms the highlighted choice; ESC = default."""
    items = ["Yes", "No"]
    selected = 0 if default else 1

    def draw_header(stdscr, max_y, max_x, search_query="", search_active=False):
        import curses
        try:
            hattr = curses.A_BOLD
            if curses.has_colors():
                hattr |= curses.color_pair(2)
            stdscr.addnstr(0, 0, "  " + text, max_x - 1, hattr)
            stdscr.addnstr(1, 0, "  ↑↓ 导航  ENTER 确认  ESC/q 返回上一步 / ↑↓ navigate  ENTER confirm  ESC/q back", max_x - 1, curses.A_DIM)
        except curses.error:
            pass
        return 3

    def draw_row(stdscr, y, i, is_cursor, max_x):
        import curses
        radio = "\u25cf" if i == selected else "\u25cb"
        arrow = "\u2192" if is_cursor else " "
        line = f" {arrow} ({radio}) {items[i]}"
        attr = curses.A_NORMAL
        if is_cursor:
            attr = curses.A_BOLD
            if curses.has_colors():
                attr |= curses.color_pair(1)
        try:
            stdscr.addnstr(y, 0, line, max_x - 1, attr)
        except curses.error:
            pass

    def on_action(action, cursor):
        if action in (NAV_SELECT, NAV_TOGGLE):
            return cursor == 0  # Yes index 0
        return default  # NAV_CANCEL → default

    try:
        import curses
        result = _run_menu(
            item_count=2,
            draw_header=draw_header,
            draw_row=draw_row,
            on_action=on_action,
            fallback=lambda: _confirm_text_fallback(text, default),
            cancel_value=default,
            initial_cursor=selected,
        )
        return bool(result)
    except Exception:
        return default


def _confirm_text_fallback(text: str, default: bool) -> bool:
    """Plain input fallback for the confirm picker."""
    if not sys.stdin.isatty():
        return default
    suffix = " [Y/n]" if default else " [y/N]"
    try:
        val = input(text + suffix + " ").strip().lower()
    except (EOFError, KeyboardInterrupt):
        return default
    if not val:
        return default
    return val in ("y", "yes")


def show_msg(title: str, lines: list[str]) -> None:
    """Print a titled block; non-TTY safe."""
    print()
    print(f"  {title}")
    for ln in lines:
        print(f"    {ln}")
    print()


# ---------------------------------------------------------------------------
# Loading spinner (network/model fetches stay inside the TUI).
# ---------------------------------------------------------------------------
# Unicode braille spinner; falls back to ASCII when the terminal can't render
# it (detected by a first-draw check inside the curses session).
_UNICODE_SPIN = ("⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏")
_ASCII_SPIN = ("|", "/", "-", "\\")


def _terminal_supports_unicode() -> bool:
    """Best-effort probe: assume UTF-8 locale unless TERM is a no-unicode one."""
    import os

    term = os.environ.get("TERM", "")
    # ASCII-only terminals (linux console, vt100) can't render braille.
    if term in ("linux", "vt100"):
        return False
    enc = (os.environ.get("LC_ALL", "") or os.environ.get("LC_CTYPE", "") or
           os.environ.get("LANG", ""))
    return "UTF-8" in enc.upper() or "UTF8" in enc.upper()


def with_loading(title: str, worker: Callable[[], Any]) -> Any:
    """Run *worker* with a curses loading spinner, returning its result.

    The spinner stays inside the TUI (no terminal flicker): a background
    thread runs *worker* while the foreground renders a braille spinner
    (ASCII fallback) plus the title. Press ESC or q to go back one step
    (raises GoBack; the worker thread is left to finish naturally and its
    result is discarded). Ctrl+C aborts the whole wizard.

    On non-TTY stdin or when curses is unavailable, prints a plain progress
    note and runs *worker* synchronously (never hangs).
    """
    if not sys.stdin.isatty():
        print(f"  {title} ...")
        return worker()

    try:
        import curses
    except Exception:
        print(f"  {title} ...")
        return worker()
    _prepare_curses()

    use_unicode = _terminal_supports_unicode()
    result: dict[str, Any] = {"value": None, "done": False}

    def _spin_frames():
        return _UNICODE_SPIN if use_unicode else _ASCII_SPIN

    def _draw(stdscr):
        curses.curs_set(0)
        if curses.has_colors():
            curses.start_color()
            curses.use_default_colors()
            curses.init_pair(1, curses.COLOR_GREEN, -1)
            curses.init_pair(2, curses.COLOR_YELLOW, -1)
        frames = _spin_frames()
        idx = 0
        stdscr.timeout(100)  # poll keys + redraw every 100ms
        while not result["done"]:
            stdscr.clear()
            max_y, max_x = stdscr.getmaxyx()
            frame = frames[idx % len(frames)]
            idx += 1
            try:
                hattr = curses.A_BOLD
                if curses.has_colors():
                    hattr |= curses.color_pair(2)
                line = f"  {frame} {title}"
                stdscr.addnstr(0, 0, line, max_x - 1, hattr)
                hint = "  按 ESC 或 q 返回上一步 / Press ESC or q to go back"
                stdscr.addnstr(2, 0, hint, max_x - 1, curses.A_DIM)
            except curses.error:
                pass
            stdscr.refresh()
            key = stdscr.getch()
            if key != -1:
                action = _decode_menu_key(stdscr, key)
                if action == NAV_ABORT:
                    raise KeyboardInterrupt  # Ctrl+C: abort the wizard
                if action == NAV_CANCEL:
                    raise GoBack()  # ESC/q: back to the previous step
        # Done: show a brief result note before exiting curses.
        stdscr.clear()
        max_y2, max_x2 = stdscr.getmaxyx()
        try:
            note = f"  \u2713 已获取模型列表 / models loaded"
            stdscr.addnstr(0, 0, note, max_x2 - 1)
        except curses.error:
            pass
        stdscr.refresh()
        time.sleep(0.4)

    try:
        t = threading.Thread(target=_run_worker, args=(worker, result), daemon=True)
        t.start()
        curses.wrapper(_draw)
        flush_stdin()
    except (KeyboardInterrupt, GoBack):
        raise  # Ctrl+C exits the wizard; ESC/q steps back
    except Exception:
        # curses error mid-spinner: degrade to a synchronous run.
        return worker()

    return result["value"]


def _run_worker(worker, result: dict[str, Any]) -> None:
    """Execute *worker* in a background thread, publishing the outcome."""
    try:
        value = worker()
    except Exception:
        value = None
    result["value"] = value
    result["done"] = True


# ---------------------------------------------------------------------------
# Numbered text fallbacks (port of Hermes' fallbacks).
# ---------------------------------------------------------------------------
def _radio_fallback(title: str, items: list[str], selected: int, cancel_returns: int) -> int:
    print(f"\n  {title}\n")
    for i, label in enumerate(items):
        marker = "(\u25cf)" if i == selected else "(\u25cb)"
        print(f"  {marker} {i + 1:>2}. {label}")
    print()
    try:
        val = input(f"  选择 / Choice [默认 default {selected + 1}]: ").strip()
        if not val:
            return selected
        idx = int(val) - 1
        if 0 <= idx < len(items):
            return idx
        return selected
    except (ValueError, KeyboardInterrupt, EOFError):
        return cancel_returns


def _checklist_fallback(title, items, selected, cancel_returns, status_fn=None) -> Set[int]:
    chosen = set(selected)
    print(f"\n  {title}\n")
    while True:
        for i, label in enumerate(items):
            marker = "[x]" if i in chosen else "[ ]"
            print(f"  {marker} {i + 1:>2}. {label}")
        if status_fn:
            text = status_fn(chosen)
            if text:
                print(f"\n  {text}")
        print()
        try:
            val = input("  切换编号 / Toggle # (or Enter to confirm): ").strip()
        except (EOFError, KeyboardInterrupt):
            return cancel_returns
        if not val:
            break
        try:
            idx = int(val) - 1
            if 0 <= idx < len(items):
                chosen.symmetric_difference_update({idx})
        except ValueError:
            pass
    return chosen


def _single_fallback(title: str, items: list[str], cancel_idx: int) -> Optional[int]:
    print(f"\n  {title}\n")
    for i, label in enumerate(items, 1):
        print(f"  {i}. {label}")
    print()
    try:
        val = input(f"  选择 / Choice [1-{len(items)}]: ").strip()
        if not val:
            return None
        idx = int(val) - 1
        if 0 <= idx < len(items) and idx < cancel_idx:
            return idx
    except (ValueError, KeyboardInterrupt, EOFError):
        pass
    return None
