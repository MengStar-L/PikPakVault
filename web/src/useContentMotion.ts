import { useCallback, useLayoutEffect, useRef, useState } from 'react'
import type { RefObject } from 'react'
import { useReducedMotion } from 'motion/react'

export type TravelDirection = 'forward' | 'back' | 'switch'
type Phase = 'idle' | 'leaving' | 'loading' | 'entering'

// Animate the content surface, never the virtual rows that own scroll offsets.
// A generation invalidates an interrupted exit so rapid navigation cannot send
// the user back to an older destination after another click or browser Back.
export function useContentMotion(scope: string, pending: boolean, scroller: RefObject<HTMLDivElement | null>, direction: TravelDirection, location: string) {
  const surface = useRef<HTMLDivElement>(null)
  const running = useRef<Animation | null>(null)
  const generation = useRef(0)
  const previousScope = useRef('')
  const previousLocation = useRef('')
  const travel = useRef(direction)
  const reduce = useReducedMotion()
  const [phase, setPhase] = useState<Phase>('idle')

  useLayoutEffect(() => {
    const ticket = ++generation.current
    running.current?.cancel()
    const el = surface.current
    if (!el) return
    if (previousScope.current !== scope) {
      scroller.current?.scrollTo({ top: 0, behavior: 'instant' })
      previousScope.current = scope
      travel.current = previousLocation.current === location ? 'switch' : direction
      previousLocation.current = location
    }
    if (pending) {
      setPhase('loading')
      return () => { generation.current++; running.current?.cancel() }
    }
    if (reduce || !el.animate) {
      setPhase('idle')
      return
    }
    setPhase('entering')
    const x = travel.current === 'switch' ? 0 : travel.current === 'back' ? -18 : 18
    const animation = el.animate([
      { opacity: 0, transform: `translate3d(${x}px, 8px, 0) scale(.99)` },
      { opacity: 1, transform: 'translate3d(0, 0, 0) scale(1)' },
    ], { duration: 420, easing: 'cubic-bezier(.16,1,.3,1)', fill: 'both' })
    running.current = animation
    animation.finished.then(() => {
      if (generation.current !== ticket) return
      animation.cancel()
      setPhase('idle')
    }).catch(() => {})
    return () => { generation.current++; running.current?.cancel() }
    // Direction is captured for each scope. Background query refreshes and
    // breadcrumbs arriving later must not restart a completed animation.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [scope, pending, reduce, scroller])

  const transition = useCallback(async (commit: () => void, nextDirection: TravelDirection) => {
    const ticket = ++generation.current
    running.current?.cancel()
    travel.current = nextDirection
    const el = surface.current
    setPhase('leaving')
    if (!reduce && el?.animate) {
      const x = nextDirection === 'back' ? 14 : nextDirection === 'forward' ? -14 : 0
      const animation = el.animate([
        { opacity: 1, transform: 'translate3d(0, 0, 0) scale(1)' },
        { opacity: 0, transform: `translate3d(${x}px, -3px, 0) scale(.99)` },
      ], { duration: 120, easing: 'cubic-bezier(.4,0,1,1)', fill: 'both' })
      running.current = animation
      try { await animation.finished } catch { return }
    }
    if (generation.current !== ticket || !surface.current?.isConnected) return
    commit()
  }, [reduce])

  const cancel = useCallback(() => {
    generation.current++
    running.current?.cancel()
    setPhase('idle')
  }, [])

  return { surface, phase, transition, cancel, reduce: !!reduce }
}
