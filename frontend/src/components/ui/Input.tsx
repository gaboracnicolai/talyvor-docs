import { forwardRef, type InputHTMLAttributes } from "react";
import clsx from "clsx";

interface InputProps extends InputHTMLAttributes<HTMLInputElement> {}

export const Input = forwardRef<HTMLInputElement, InputProps>(
  ({ className, ...rest }, ref) => (
    <input
      ref={ref}
      className={clsx(
        "h-9 w-full rounded-md border border-border bg-bg px-3 text-sm",
        "text-text placeholder:text-muted",
        "focus:border-accent focus:outline-none focus:ring-2 focus:ring-accent",
        className,
      )}
      {...rest}
    />
  ),
);
Input.displayName = "Input";
