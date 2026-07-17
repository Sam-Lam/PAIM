import type { ButtonHTMLAttributes, ComponentType, ReactNode } from "react";
import { Spinner } from "./Spinner";

export type ButtonVariant = "primary" | "secondary" | "ghost" | "danger";
export type ButtonSize = "sm" | "md" | "lg";

export interface ButtonProps extends ButtonHTMLAttributes<HTMLButtonElement> {
  variant?: ButtonVariant;
  size?: ButtonSize;
  icon?: ComponentType<{ className?: string }>;
  loading?: boolean;
  children?: ReactNode;
}

const VARIANT: Record<ButtonVariant, string> = {
  primary: "bg-blue-600 text-white hover:bg-blue-500 disabled:bg-blue-600/50",
  secondary:
    "border border-zinc-700 bg-zinc-800/60 text-zinc-100 hover:bg-zinc-800 hover:border-zinc-600 disabled:opacity-50",
  ghost: "text-zinc-300 hover:bg-zinc-800/70 hover:text-zinc-100 disabled:opacity-50",
  danger: "bg-red-600 text-white hover:bg-red-500 disabled:bg-red-600/50",
};

const SIZE: Record<ButtonSize, string> = {
  sm: "h-7 gap-1.5 px-2.5 text-xs",
  md: "h-8 gap-2 px-3 text-[13px]",
  lg: "h-10 gap-2 px-4 text-sm",
};

const ICON_SIZE: Record<ButtonSize, string> = { sm: "h-3.5 w-3.5", md: "h-4 w-4", lg: "h-5 w-5" };

export function Button({
  variant = "secondary",
  size = "md",
  icon: Icon,
  loading = false,
  disabled,
  children,
  className = "",
  ...rest
}: ButtonProps) {
  return (
    <button
      {...rest}
      disabled={disabled || loading}
      // Buttons live inside draggable chrome; keep them clickable.
      style={{ "--wails-draggable": "no-drag", ...(rest.style ?? {}) } as React.CSSProperties}
      className={`inline-flex select-none items-center justify-center rounded-md font-medium transition-colors disabled:cursor-not-allowed ${SIZE[size]} ${VARIANT[variant]} ${className}`}
    >
      {loading ? <Spinner size={size === "sm" ? 12 : 14} /> : Icon ? <Icon className={ICON_SIZE[size]} /> : null}
      {children}
    </button>
  );
}
