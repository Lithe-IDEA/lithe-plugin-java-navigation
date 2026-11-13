// Signatures read from lib-1.0.jar by Java Navigation; no sources are attached.
package com.example.lib;

public @interface Schema {
    String description() default "";
    Schema.RequiredMode requiredMode() default Schema.RequiredMode.AUTO;
    Class<?> implementation() default Void.class;
    String[] allowableValues() default {};
    int maxLength() default 2147483647;
    public enum RequiredMode {
        AUTO,
        REQUIRED,
        NOT_REQUIRED;
    }
}
