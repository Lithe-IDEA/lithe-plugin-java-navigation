package com.example.lib;

import java.lang.annotation.ElementType;
import java.lang.annotation.Retention;
import java.lang.annotation.RetentionPolicy;
import java.lang.annotation.Target;

@Retention(RetentionPolicy.RUNTIME)
@Target({ElementType.TYPE, ElementType.FIELD})
public @interface Schema {
    String description() default "";

    RequiredMode requiredMode() default RequiredMode.AUTO;

    Class<?> implementation() default Void.class;

    String[] allowableValues() default {};

    int maxLength() default Integer.MAX_VALUE;

    enum RequiredMode {
        AUTO,
        REQUIRED,
        NOT_REQUIRED
    }
}
