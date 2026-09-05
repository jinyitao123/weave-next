import type { HeroBrandMarkOwnerProps } from '@deepseek-ai/dsh-client-ui-conversation/client'
import type { SidebarBrandMarkOwnerProps } from '@deepseek-ai/dsh-client-ui-sidebar/client'

type WorkbenchBrandMarkProps = HeroBrandMarkOwnerProps & SidebarBrandMarkOwnerProps

/** Render the compact Weave mark requested by the host surface. */
export function WorkbenchBrandMark({ size, className }: WorkbenchBrandMarkProps) {
  return (
    <svg
      aria-hidden="true"
      className={className}
      height={size}
      viewBox="0 0 32 32"
      width={size}
      xmlns="http://www.w3.org/2000/svg"
    >
      <rect fill="currentColor" height="32" rx="9" width="32" />
      <path
        d="M7 9.5 11.1 23h3.2l1.7-6.2 1.7 6.2h3.2L25 9.5h-3.3l-2.4 9-1.8-6.6h-3l-1.8 6.6-2.4-9H7Z"
        fill="white"
      />
    </svg>
  )
}

/** Render the build-configured product name beside the mark. */
export function WorkbenchBrandName() {
  return <span>{process.env.DSH_CLIENT_TITLE}</span>
}
