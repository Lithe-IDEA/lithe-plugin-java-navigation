package com.example.lib;

public enum Level {
    LOW(1) {
        @Override
        public String label() {
            return "low";
        }
    },
    HIGH(2) {
        @Override
        public String label() {
            return "high";
        }
    };

    private final int weight;

    Level(int weight) {
        this.weight = weight;
    }

    public int weight() {
        return weight;
    }

    public abstract String label();
}
